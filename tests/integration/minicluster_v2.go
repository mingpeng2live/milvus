// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package integration

import (
	"context"
	"fmt"
	"math"
	"net"
	"path"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus/internal/coordinator/coordclient"
	grpcdatacoord "github.com/milvus-io/milvus/internal/distributed/datacoord"
	grpcdatacoordclient "github.com/milvus-io/milvus/internal/distributed/datacoord/client"
	grpcdatanode "github.com/milvus-io/milvus/internal/distributed/datanode"
	grpcdatanodeclient "github.com/milvus-io/milvus/internal/distributed/datanode/client"
	grpcproxy "github.com/milvus-io/milvus/internal/distributed/proxy"
	grpcproxyclient "github.com/milvus-io/milvus/internal/distributed/proxy/client"
	grpcquerycoord "github.com/milvus-io/milvus/internal/distributed/querycoord"
	grpcquerycoordclient "github.com/milvus-io/milvus/internal/distributed/querycoord/client"
	grpcquerynode "github.com/milvus-io/milvus/internal/distributed/querynode"
	grpcquerynodeclient "github.com/milvus-io/milvus/internal/distributed/querynode/client"
	grpcrootcoord "github.com/milvus-io/milvus/internal/distributed/rootcoord"
	grpcrootcoordclient "github.com/milvus-io/milvus/internal/distributed/rootcoord/client"
	"github.com/milvus-io/milvus/internal/distributed/streaming"
	"github.com/milvus-io/milvus/internal/distributed/streamingnode"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/streamingcoord/server/broadcaster/registry"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/dependency"
	"github.com/milvus-io/milvus/internal/util/hookutil"
	"github.com/milvus-io/milvus/internal/util/streamingutil"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/util/etcd"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

var params *paramtable.ComponentParam = paramtable.Get()

var (
	initOnce  sync.Once
	configMap map[string]string
)

func DefaultParams() map[string]string {
	initOnce.Do(func() {
		testPath := fmt.Sprintf("integration-test-%d", time.Now().Unix())

		// Notice: don't use ParamItem.Key here, the config key will be empty before param table init
		configMap = map[string]string{
			"mq.type":                           "rocksmq",
			"etcd.rootPath":                     testPath,
			"msgChannel.chanNamePrefix.cluster": testPath,
			"minio.rootPath":                    testPath,
			"localStorage.path":                 path.Join("/tmp", testPath),
			"common.storageType":                "local",
			"dataNode.memory.forceSyncEnable":   "false", // local execution will print too many logs
			"common.gracefulStopTimeout":        "30",
		}
	})

	return configMap
}

type MiniClusterV2 struct {
	ctx context.Context

	mu sync.RWMutex

	params map[string]string

	factory      dependency.Factory
	ChunkManager storage.ChunkManager

	EtcdCli *clientv3.Client

	Proxy      *grpcproxy.Server
	DataCoord  *grpcdatacoord.Server
	RootCoord  *grpcrootcoord.Server
	QueryCoord *grpcquerycoord.Server

	DataCoordClient  types.DataCoordClient
	RootCoordClient  types.RootCoordClient
	QueryCoordClient types.QueryCoordClient

	MilvusClient    milvuspb.MilvusServiceClient
	ProxyClient     types.ProxyClient
	DataNodeClient  types.DataNodeClient
	QueryNodeClient types.QueryNodeClient

	DataNode      *grpcdatanode.Server
	StreamingNode *streamingnode.Server
	QueryNode     *grpcquerynode.Server

	MetaWatcher    MetaWatcher
	ptmu           sync.Mutex
	querynodes     []*grpcquerynode.Server
	qnid           atomic.Int64
	datanodes      []*grpcdatanode.Server
	dnid           atomic.Int64
	streamingnodes []*streamingnode.Server

	clientConn *grpc.ClientConn
	Extension  *ReportChanExtension
}

type OptionV2 func(cluster *MiniClusterV2)

func StartMiniClusterV2(ctx context.Context, opts ...OptionV2) (*MiniClusterV2, error) {
	cluster := &MiniClusterV2{
		ctx:  ctx,
		qnid: *atomic.NewInt64(10000),
		dnid: *atomic.NewInt64(20000),
	}
	paramtable.Init()
	cluster.Extension = InitReportExtension()

	cluster.params = DefaultParams()
	for _, opt := range opts {
		opt(cluster)
	}
	for k, v := range cluster.params {
		params.Save(k, v)
	}
	paramtable.SetRole(typeutil.StandaloneRole)

	// setup etcd client
	etcdConfig := &paramtable.Get().EtcdCfg
	etcdCli, err := etcd.GetEtcdClient(
		etcdConfig.UseEmbedEtcd.GetAsBool(),
		etcdConfig.EtcdUseSSL.GetAsBool(),
		etcdConfig.Endpoints.GetAsStrings(),
		etcdConfig.EtcdTLSCert.GetValue(),
		etcdConfig.EtcdTLSKey.GetValue(),
		etcdConfig.EtcdTLSCACert.GetValue(),
		etcdConfig.EtcdTLSMinVersion.GetValue())
	if err != nil {
		return nil, err
	}
	cluster.EtcdCli = etcdCli

	coordclient.ResetRegistration()
	registry.ResetRegistration()
	streaming.Init()

	cluster.MetaWatcher = &EtcdMetaWatcher{
		rootPath: etcdConfig.RootPath.GetValue(),
		etcdCli:  cluster.EtcdCli,
	}

	ports, err := cluster.GetAvailablePorts(7)
	if err != nil {
		return nil, err
	}
	log.Info("minicluster ports", zap.Ints("ports", ports))
	params.RootCoordGrpcServerCfg.IP = "localhost"
	params.QueryCoordGrpcServerCfg.IP = "localhost"
	params.DataCoordGrpcServerCfg.IP = "localhost"
	params.ProxyGrpcServerCfg.IP = "localhost"
	params.QueryNodeGrpcServerCfg.IP = "localhost"
	params.DataNodeGrpcServerCfg.IP = "localhost"
	params.StreamingNodeGrpcServerCfg.IP = "localhost"
	params.Save(params.RootCoordGrpcServerCfg.Port.Key, fmt.Sprint(ports[0]))
	params.Save(params.DataCoordGrpcServerCfg.Port.Key, fmt.Sprint(ports[1]))
	params.Save(params.QueryCoordGrpcServerCfg.Port.Key, fmt.Sprint(ports[2]))
	params.Save(params.DataNodeGrpcServerCfg.Port.Key, fmt.Sprint(ports[3]))
	params.Save(params.QueryNodeGrpcServerCfg.Port.Key, fmt.Sprint(ports[4]))
	params.Save(params.ProxyGrpcServerCfg.Port.Key, fmt.Sprint(ports[6]))

	// setup clients
	cluster.RootCoordClient, err = grpcrootcoordclient.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	cluster.DataCoordClient, err = grpcdatacoordclient.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	cluster.QueryCoordClient, err = grpcquerycoordclient.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	cluster.ProxyClient, err = grpcproxyclient.NewClient(ctx, paramtable.Get().ProxyGrpcClientCfg.GetInternalAddress(), 0)
	if err != nil {
		return nil, err
	}
	cluster.DataNodeClient, err = grpcdatanodeclient.NewClient(ctx, paramtable.Get().DataNodeGrpcClientCfg.GetAddress(), 0, false)
	if err != nil {
		return nil, err
	}
	cluster.QueryNodeClient, err = grpcquerynodeclient.NewClient(ctx, paramtable.Get().QueryNodeGrpcClientCfg.GetAddress(), 0)
	if err != nil {
		return nil, err
	}

	// setup servers
	cluster.factory = dependency.MockDefaultFactory(true, params)
	chunkManager, err := cluster.factory.NewPersistentStorageChunkManager(cluster.ctx)
	if err != nil {
		return nil, err
	}
	cluster.ChunkManager = chunkManager

	cluster.RootCoord, err = grpcrootcoord.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	cluster.DataCoord, err = grpcdatacoord.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	cluster.QueryCoord, err = grpcquerycoord.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	cluster.Proxy, err = grpcproxy.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	cluster.DataNode, err = grpcdatanode.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	if streamingutil.IsStreamingServiceEnabled() {
		cluster.StreamingNode, err = streamingnode.NewServer(ctx, cluster.factory)
		if err != nil {
			return nil, err
		}
	}
	cluster.QueryNode, err = grpcquerynode.NewServer(ctx, cluster.factory)
	if err != nil {
		return nil, err
	}
	return cluster, nil
}

func (cluster *MiniClusterV2) AddQueryNodes(k int) []*grpcquerynode.Server {
	servers := make([]*grpcquerynode.Server, k)
	for i := 0; i < k; i++ {
		servers = append(servers, cluster.AddQueryNode())
	}
	return servers
}

func (cluster *MiniClusterV2) AddQueryNode() *grpcquerynode.Server {
	cluster.ptmu.Lock()
	defer cluster.ptmu.Unlock()
	cluster.qnid.Inc()
	id := cluster.qnid.Load()
	oid := paramtable.GetNodeID()
	log.Info(fmt.Sprintf("adding extra querynode with id:%d", id))
	paramtable.SetNodeID(id)
	node, err := grpcquerynode.NewServer(context.TODO(), cluster.factory)
	if err != nil {
		return nil
	}
	runComponent(node)
	paramtable.SetNodeID(oid)

	req := &milvuspb.GetComponentStatesRequest{}
	resp, err := node.GetComponentStates(context.TODO(), req)
	if err != nil {
		return nil
	}
	log.Info(fmt.Sprintf("querynode %d ComponentStates:%v", id, resp))
	cluster.querynodes = append(cluster.querynodes, node)
	return node
}

func (cluster *MiniClusterV2) AddDataNode() *grpcdatanode.Server {
	cluster.ptmu.Lock()
	defer cluster.ptmu.Unlock()
	cluster.qnid.Inc()
	id := cluster.qnid.Load()
	oid := paramtable.GetNodeID()
	log.Info(fmt.Sprintf("adding extra datanode with id:%d", id))
	paramtable.SetNodeID(id)
	node, err := grpcdatanode.NewServer(context.TODO(), cluster.factory)
	if err != nil {
		return nil
	}
	runComponent(node)
	paramtable.SetNodeID(oid)

	req := &milvuspb.GetComponentStatesRequest{}
	resp, err := node.GetComponentStates(context.TODO(), req)
	if err != nil {
		return nil
	}
	log.Info(fmt.Sprintf("datanode %d ComponentStates:%v", id, resp))
	cluster.datanodes = append(cluster.datanodes, node)
	return node
}

func (cluster *MiniClusterV2) AddStreamingNode() {
	cluster.ptmu.Lock()
	defer cluster.ptmu.Unlock()

	node, err := streamingnode.NewServer(context.TODO(), cluster.factory)
	if err != nil {
		panic(err)
	}
	runComponent(node)
	cluster.streamingnodes = append(cluster.streamingnodes, node)
}

func (cluster *MiniClusterV2) Start() error {
	log.Info("mini cluster start")
	runComponent(cluster.RootCoord)
	runComponent(cluster.DataCoord)
	runComponent(cluster.QueryCoord)
	runComponent(cluster.DataNode)
	runComponent(cluster.QueryNode)
	runComponent(cluster.Proxy)

	ctx2, cancel := context.WithTimeout(context.Background(), time.Second*120)
	defer cancel()
	healthy := false
	for !healthy {
		checkHealthResp, _ := cluster.Proxy.CheckHealth(ctx2, &milvuspb.CheckHealthRequest{})
		healthy = checkHealthResp.IsHealthy
		time.Sleep(time.Second * 1)
	}
	if !healthy {
		return errors.New("minicluster is not healthy after 120s")
	}

	if streamingutil.IsStreamingServiceEnabled() {
		paramtable.SetLocalComponentEnabled(typeutil.StreamingNodeRole)
		runComponent(cluster.StreamingNode)
	}

	port := params.ProxyGrpcServerCfg.Port.GetAsInt()
	var err error
	cluster.clientConn, err = grpc.DialContext(cluster.ctx, fmt.Sprintf("localhost:%d", port), getGrpcDialOpt()...)
	if err != nil {
		return err
	}

	cluster.MilvusClient = milvuspb.NewMilvusServiceClient(cluster.clientConn)
	log.Info("minicluster started")
	return nil
}

func (cluster *MiniClusterV2) StopRootCoord() {
	if err := cluster.RootCoord.Stop(); err != nil {
		panic(err)
	}
	cluster.RootCoord = nil
}

func (cluster *MiniClusterV2) StartRootCoord() {
	if cluster.RootCoord == nil {
		coordclient.ResetRootCoordRegistration()
		var err error
		if cluster.RootCoord, err = grpcrootcoord.NewServer(cluster.ctx, cluster.factory); err != nil {
			panic(err)
		}
		runComponent(cluster.RootCoord)
	}
}

func (cluster *MiniClusterV2) StopDataCoord() {
	if err := cluster.DataCoord.Stop(); err != nil {
		panic(err)
	}
	cluster.DataCoord = nil
}

func (cluster *MiniClusterV2) StartDataCoord() {
	if cluster.DataCoord == nil {
		coordclient.ResetRootCoordRegistration()
		var err error
		if cluster.DataCoord, err = grpcdatacoord.NewServer(cluster.ctx, cluster.factory); err != nil {
			panic(err)
		}
		runComponent(cluster.DataCoord)
	}
}

func (cluster *MiniClusterV2) StopQueryCoord() {
	if err := cluster.QueryCoord.Stop(); err != nil {
		panic(err)
	}
	cluster.QueryCoord = nil
}

func (cluster *MiniClusterV2) StartQueryCoord() {
	if cluster.QueryCoord == nil {
		coordclient.ResetQueryCoordRegistration()
		var err error
		if cluster.QueryCoord, err = grpcquerycoord.NewServer(cluster.ctx, cluster.factory); err != nil {
			panic(err)
		}
		runComponent(cluster.QueryCoord)
	}
}

func getGrpcDialOpt() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithBlock(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                5 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  100 * time.Millisecond,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   3 * time.Second,
			},
			MinConnectTimeout: 3 * time.Second,
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(grpc_retry.UnaryClientInterceptor(
			grpc_retry.WithMax(6),
			grpc_retry.WithBackoff(func(attempt uint) time.Duration {
				return 60 * time.Millisecond * time.Duration(math.Pow(3, float64(attempt)))
			}),
			grpc_retry.WithCodes(codes.Unavailable, codes.ResourceExhausted)),
		),
	}
}

func (cluster *MiniClusterV2) Stop() error {
	log.Info("mini cluster stop")
	if cluster.clientConn != nil {
		cluster.clientConn.Close()
	}
	if cluster.RootCoord != nil {
		cluster.RootCoord.Stop()
		log.Info("mini cluster rootCoord stopped")
	}

	if cluster.DataCoord != nil {
		cluster.DataCoord.Stop()
		log.Info("mini cluster dataCoord stopped")
	}
	if cluster.QueryCoord != nil {
		cluster.QueryCoord.Stop()
		log.Info("mini cluster queryCoord stopped")
	}
	if cluster.Proxy != nil {
		cluster.Proxy.Stop()
		log.Info("mini cluster proxy stopped")
	}

	cluster.StopAllDataNodes()
	cluster.StopAllStreamingNodes()
	cluster.StopAllQueryNodes()

	cluster.EtcdCli.KV.Delete(cluster.ctx, params.EtcdCfg.RootPath.GetValue(), clientv3.WithPrefix())
	defer cluster.EtcdCli.Close()

	if cluster.ChunkManager == nil {
		chunkManager, err := cluster.factory.NewPersistentStorageChunkManager(cluster.ctx)
		if err != nil {
			log.Warn("fail to create chunk manager to clean test data", zap.Error(err))
		} else {
			cluster.ChunkManager = chunkManager
		}
	}
	cluster.ChunkManager.RemoveWithPrefix(cluster.ctx, cluster.ChunkManager.RootPath())
	streaming.Release()
	return nil
}

func (cluster *MiniClusterV2) GetAllQueryNodes() []*grpcquerynode.Server {
	ret := make([]*grpcquerynode.Server, 0)
	ret = append(ret, cluster.QueryNode)
	ret = append(ret, cluster.querynodes...)
	return ret
}

func (cluster *MiniClusterV2) StopAllQueryNodes() {
	cluster.QueryNode.Stop()
	log.Info("mini cluster main queryNode stopped")
	numExtraQN := len(cluster.querynodes)
	for _, node := range cluster.querynodes {
		node.Stop()
	}
	cluster.querynodes = nil
	log.Info(fmt.Sprintf("mini cluster stopped %d extra querynode", numExtraQN))
}

func (cluster *MiniClusterV2) StopAllDataNodes() {
	cluster.DataNode.Stop()
	log.Info("mini cluster main dataNode stopped")
	numExtraDN := len(cluster.datanodes)
	for _, node := range cluster.datanodes {
		node.Stop()
	}
	cluster.datanodes = nil
	log.Info(fmt.Sprintf("mini cluster stopped %d extra datanode", numExtraDN))
}

func (cluster *MiniClusterV2) StopAllStreamingNodes() {
	if cluster.StreamingNode != nil {
		cluster.StreamingNode.Stop()
		log.Info("mini cluster main streamingnode stopped")
	}
	for _, node := range cluster.streamingnodes {
		node.Stop()
	}
	log.Info(fmt.Sprintf("mini cluster stopped %d streaming nodes", len(cluster.streamingnodes)))
	cluster.streamingnodes = nil
}

func (cluster *MiniClusterV2) GetContext() context.Context {
	return cluster.ctx
}

func (cluster *MiniClusterV2) GetFactory() dependency.Factory {
	return cluster.factory
}

func (cluster *MiniClusterV2) GetAvailablePorts(n int) ([]int, error) {
	ports := typeutil.NewSet[int]()
	for ports.Len() < n {
		port, err := cluster.GetAvailablePort()
		if err != nil {
			return nil, err
		}
		ports.Insert(port)
	}
	return ports.Collect(), nil
}

func (cluster *MiniClusterV2) GetAvailablePort() (int, error) {
	address, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:0", "0.0.0.0"))
	if err != nil {
		return 0, err
	}
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func InitReportExtension() *ReportChanExtension {
	e := NewReportChanExtension()
	hookutil.InitOnceHook()
	hookutil.SetTestExtension(e)
	return e
}

type ReportChanExtension struct {
	reportChan chan any
}

func NewReportChanExtension() *ReportChanExtension {
	return &ReportChanExtension{
		reportChan: make(chan any),
	}
}

func (r *ReportChanExtension) Report(info any) int {
	select {
	case r.reportChan <- info:
	default:
	}
	return 1
}

func (r *ReportChanExtension) ReportRefused(ctx context.Context, req interface{}, resp interface{}, err error, fullMethod string) error {
	return nil
}

func (r *ReportChanExtension) GetReportChan() <-chan any {
	return r.reportChan
}

type component interface {
	Prepare() error
	Run() error
}

func runComponent(c component) {
	if err := c.Prepare(); err != nil {
		panic(err)
	}
	if err := c.Run(); err != nil {
		panic(err)
	}
}
