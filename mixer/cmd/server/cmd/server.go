// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	_ "expvar" // For /debug/vars registration. Note: temporary, NOT for general use
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	_ "net/http/pprof" // For profiling / performance investigations
	"strings"
	"time"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	ot "github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	mixerpb "istio.io/api/mixer/v1"
	"istio.io/istio/mixer/cmd/shared"
	adptr "istio.io/istio/mixer/pkg/adapter"
	"istio.io/istio/mixer/pkg/adapterManager"
	"istio.io/istio/mixer/pkg/api"
	"istio.io/istio/mixer/pkg/aspect"
	"istio.io/istio/mixer/pkg/config"
	"istio.io/istio/mixer/pkg/config/store"
	"istio.io/istio/mixer/pkg/expr"
	"istio.io/istio/mixer/pkg/il/evaluator"
	"istio.io/istio/mixer/pkg/pool"
	mixerRuntime "istio.io/istio/mixer/pkg/runtime"
	"istio.io/istio/mixer/pkg/template"
	"istio.io/istio/mixer/pkg/tracing"
	"istio.io/istio/mixer/pkg/version"
)

const (
	metricsPath = "/metrics"
	versionPath = "/version"
)

type serverArgs struct {
	maxMessageSize                uint
	maxConcurrentStreams          uint
	apiWorkerPoolSize             int
	adapterWorkerPoolSize         int
	expressionEvalCacheSize       int
	port                          uint16
	configAPIPort                 uint16
	monitoringPort                uint16
	singleThreaded                bool
	compressedPayload             bool
	zipkinURL                     string
	jaegerURL                     string
	logTraceSpans                 bool
	serverCertFile                string
	serverKeyFile                 string
	clientCertFiles               string
	configStoreURL                string
	configStore2URL               string
	configDefaultNamespace        string
	configFetchIntervalSec        uint
	configIdentityAttribute       string
	configIdentityAttributeDomain string
	stringTablePurgeLimit         int

	// @deprecated
	serviceConfigFile string
	// @deprecated
	globalConfigFile string
}

func (sa *serverArgs) String() string {
	var b bytes.Buffer
	s := *sa
	b.WriteString(fmt.Sprint("maxMessageSize: ", s.maxMessageSize, "\n"))
	b.WriteString(fmt.Sprint("maxConcurrentStreams: ", s.maxConcurrentStreams, "\n"))
	b.WriteString(fmt.Sprint("apiWorkerPoolSize: ", s.apiWorkerPoolSize, "\n"))
	b.WriteString(fmt.Sprint("adapterWorkerPoolSize: ", s.adapterWorkerPoolSize, "\n"))
	b.WriteString(fmt.Sprint("expressionEvalCacheSize: ", s.expressionEvalCacheSize, "\n"))
	b.WriteString(fmt.Sprint("port: ", s.port, "\n"))
	b.WriteString(fmt.Sprint("configAPIPort: ", s.configAPIPort, "\n"))
	b.WriteString(fmt.Sprint("monitoringPort: ", s.monitoringPort, "\n"))
	b.WriteString(fmt.Sprint("singleThreaded: ", s.singleThreaded, "\n"))
	b.WriteString(fmt.Sprint("compressedPayload: ", s.compressedPayload, "\n"))
	b.WriteString(fmt.Sprint("zipkinURL: ", s.zipkinURL, "\n"))
	b.WriteString(fmt.Sprint("jaegerURL: ", s.jaegerURL, "\n"))
	b.WriteString(fmt.Sprint("logTraceSpans: ", s.logTraceSpans, "\n"))
	b.WriteString(fmt.Sprint("serverCertFile: ", s.serverCertFile, "\n"))
	b.WriteString(fmt.Sprint("serverKeyFile: ", s.serverKeyFile, "\n"))
	b.WriteString(fmt.Sprint("clientCertFiles: ", s.clientCertFiles, "\n"))
	b.WriteString(fmt.Sprint("configStoreURL: ", s.configStoreURL, "\n"))
	b.WriteString(fmt.Sprint("configStore2URL: ", s.configStore2URL, "\n"))
	b.WriteString(fmt.Sprint("configDefaultNamespace: ", s.configDefaultNamespace, "\n"))
	b.WriteString(fmt.Sprint("configFetchIntervalSec: ", s.configFetchIntervalSec, "\n"))
	b.WriteString(fmt.Sprint("configIdentityAttribute: ", s.configIdentityAttribute, "\n"))
	b.WriteString(fmt.Sprint("configIdentityAttributeDomain: ", s.configIdentityAttributeDomain, "\n"))
	b.WriteString(fmt.Sprint("stringTablePurgeLimit: ", s.stringTablePurgeLimit, "\n"))
	return b.String()
}

// ServerContext exports Mixer Grpc server and internal GoroutinePools.
type ServerContext struct {
	GP        *pool.GoroutinePool
	AdapterGP *pool.GoroutinePool
	Server    *grpc.Server
	Closers   []io.Closer
}

func serverCmd(info map[string]template.Info, adapters []adptr.InfoFn, legacyAdapters []adptr.RegisterFn, printf, fatalf shared.FormatFn) *cobra.Command {
	sa := &serverArgs{}
	serverCmd := cobra.Command{
		Use:   "server",
		Short: "Starts Mixer as a server",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if sa.apiWorkerPoolSize <= 0 {
				return fmt.Errorf("api worker pool size must be >= 0 and <= 2^31-1, got pool size %d", sa.apiWorkerPoolSize)
			}

			if sa.adapterWorkerPoolSize <= 0 {
				return fmt.Errorf("adapter worker pool size must be >= 0 and <= 2^31-1, got pool size %d", sa.adapterWorkerPoolSize)
			}

			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			runServer(sa, info, adapters, legacyAdapters, printf, fatalf)
		},
	}

	// TODO: need to pick appropriate defaults for all these settings below

	serverCmd.PersistentFlags().Uint16VarP(&sa.port, "port", "p", 9091, "TCP port to use for Mixer's gRPC API")
	serverCmd.PersistentFlags().Uint16Var(&sa.monitoringPort, "monitoringPort", 9093, "HTTP port to use for the exposing mixer self-monitoring information")
	serverCmd.PersistentFlags().Uint16VarP(&sa.configAPIPort, "configAPIPort", "", 9094, "HTTP port to use for Mixer's Configuration API")
	serverCmd.PersistentFlags().UintVarP(&sa.maxMessageSize, "maxMessageSize", "", 1024*1024, "Maximum size of individual gRPC messages")
	serverCmd.PersistentFlags().UintVarP(&sa.maxConcurrentStreams, "maxConcurrentStreams", "", 1024, "Maximum number of outstanding RPCs per connection")
	serverCmd.PersistentFlags().IntVarP(&sa.apiWorkerPoolSize, "apiWorkerPoolSize", "", 1024, "Max number of goroutines in the API worker pool")
	serverCmd.PersistentFlags().IntVarP(&sa.adapterWorkerPoolSize, "adapterWorkerPoolSize", "", 1024, "Max number of goroutines in the adapter worker pool")
	// TODO: what is the right default value for expressionEvalCacheSize.
	serverCmd.PersistentFlags().IntVarP(&sa.expressionEvalCacheSize, "expressionEvalCacheSize", "", evaluator.DefaultCacheSize,
		"Number of entries in the expression cache")
	serverCmd.PersistentFlags().BoolVarP(&sa.singleThreaded, "singleThreaded", "", false,
		"If true, each request to Mixer will be executed in a single go routine (useful for debugging)")
	serverCmd.PersistentFlags().BoolVarP(&sa.compressedPayload, "compressedPayload", "", false, "Whether to compress gRPC messages")

	serverCmd.PersistentFlags().StringVarP(&sa.serverCertFile, "serverCertFile", "", "", "The TLS cert file")
	_ = serverCmd.MarkPersistentFlagFilename("serverCertFile")

	serverCmd.PersistentFlags().StringVarP(&sa.serverKeyFile, "serverKeyFile", "", "", "The TLS key file")
	_ = serverCmd.MarkPersistentFlagFilename("serverKeyFile")

	serverCmd.PersistentFlags().StringVarP(&sa.clientCertFiles, "clientCertFiles", "", "", "A set of comma-separated client X509 cert files")

	// DEPRECATED FLAG (traceOutput). TO BE REMOVED IN SUBSEQUENT RELEASES.
	serverCmd.PersistentFlags().StringVarP(&sa.zipkinURL, "traceOutput", "", "", "DEPRECATED. URL of zipkin collector (example: 'http://zipkin:9411/api/v1/spans'")
	serverCmd.PersistentFlags().MarkDeprecated("traceOutput", "please use one (or more) of the following flags: --zipkinURL, --jaegerURL, or --logTraceSpans")

	serverCmd.PersistentFlags().StringVarP(&sa.zipkinURL, "zipkinURL", "", "",
		"URL of zipkin collector (example: 'http://zipkin:9411/api/v1/spans'). This enables tracing for Mixer itself.")
	serverCmd.PersistentFlags().StringVarP(&sa.jaegerURL, "jaegerURL", "", "",
		"URL of jaeger HTTP collector (example: 'http://jaeger:14268/api/traces?format=jaeger.thrift'). This enables tracing for Mixer itself.")
	serverCmd.PersistentFlags().BoolVarP(&sa.logTraceSpans, "logTraceSpans", "", false,
		"Whether or not to log Mixer trace spans. This enables tracing for Mixer itself.")

	serverCmd.PersistentFlags().StringVarP(&sa.configStoreURL, "configStoreURL", "", "",
		"URL of the config store. May be fs:// for file system, or redis:// for redis url")

	serverCmd.PersistentFlags().StringVarP(&sa.configStore2URL, "configStore2URL", "", "",
		"URL of the config store. Use k8s://path_to_kubeconfig or fs:// for file system. If path_to_kubeconfig is empty, in-cluster kubeconfig is used.")

	serverCmd.PersistentFlags().StringVarP(&sa.configDefaultNamespace, "configDefaultNamespace", "", mixerRuntime.DefaultConfigNamespace,
		"Namespace used to store mesh wide configuration.")

	// Hide configIdentityAttribute and configIdentityAttributeDomain until we have a need to expose it.
	// These parameters ensure that rest of Mixer makes no assumptions about specific identity attribute.
	// Rules selection is based on scopes.
	serverCmd.PersistentFlags().StringVarP(&sa.configIdentityAttribute, "configIdentityAttribute", "", "destination.service",
		"Attribute that is used to identify applicable scopes.")
	if err := serverCmd.PersistentFlags().MarkHidden("configIdentityAttribute"); err != nil {
		fatalf("unable to hide: %v", err)
	}
	serverCmd.PersistentFlags().StringVarP(&sa.configIdentityAttributeDomain, "configIdentityAttributeDomain", "", "svc.cluster.local",
		"The domain to which all values of the configIdentityAttribute belong. For kubernetes services it is svc.cluster.local")
	if err := serverCmd.PersistentFlags().MarkHidden("configIdentityAttributeDomain"); err != nil {
		fatalf("unable to hide: %v", err)
	}

	serverCmd.PersistentFlags().IntVar(&sa.stringTablePurgeLimit, "stringTablePurgeLimit", 1024, "Upper limit for String table size to purge at.")
	// serviceConfig and gobalConfig are for compatibility only
	serverCmd.PersistentFlags().StringVarP(&sa.serviceConfigFile, "serviceConfigFile", "", "", "Combined Service Config")
	serverCmd.PersistentFlags().StringVarP(&sa.globalConfigFile, "globalConfigFile", "", "", "Global Config")

	serverCmd.PersistentFlags().UintVarP(&sa.configFetchIntervalSec, "configFetchInterval", "", 5, "Configuration fetch interval in seconds")
	return &serverCmd
}

// configStore - given config this function returns a KeyValueStore
// It provides a compatibility layer so one can continue using serviceConfigFile and globalConfigFile flags
// until they are removed.
func configStore(url, serviceConfigFile, globalConfigFile string, printf, fatalf shared.FormatFn) (s store.KeyValueStore) {
	var err error
	if url != "" {
		registry := store.NewRegistry(config.StoreInventory()...)
		if s, err = registry.NewStore(url); err != nil {
			fatalf("Failed to get config store: %v", err)
		}
		return s
	}
	if serviceConfigFile == "" || globalConfigFile == "" {
		fatalf("Missing configStoreURL")
	}
	printf("*** serviceConfigFile and globalConfigFile are deprecated, use configStoreURL")
	if s, err = config.NewCompatFSStore(globalConfigFile, serviceConfigFile); err != nil {
		fatalf("Failed to get config store: %v", err)
	}
	return s
}

func setupServer(sa *serverArgs, info map[string]template.Info, adapters []adptr.InfoFn,
	legacyAdapters []adptr.RegisterFn, printf, fatalf shared.FormatFn) *ServerContext {
	var err error
	apiPoolSize := sa.apiWorkerPoolSize
	adapterPoolSize := sa.adapterWorkerPoolSize
	expressionEvalCacheSize := sa.expressionEvalCacheSize

	gp := pool.NewGoroutinePool(apiPoolSize, sa.singleThreaded)
	gp.AddWorkers(apiPoolSize)

	adapterGP := pool.NewGoroutinePool(adapterPoolSize, sa.singleThreaded)
	adapterGP.AddWorkers(adapterPoolSize)

	closers := []io.Closer{gp, adapterGP}

	// Old and new runtime maintain their own evaluators with
	// configs and attribute vocabularies.
	var ilEvalForLegacy *evaluator.IL
	var eval expr.Evaluator
	var evalForLegacy expr.Evaluator
	eval, err = evaluator.NewILEvaluator(expressionEvalCacheSize, sa.stringTablePurgeLimit)
	if err != nil {
		fatalf("Failed to create IL expression evaluator with cache size %d: %v", expressionEvalCacheSize, err)
	}
	ilEvalForLegacy, err = evaluator.NewILEvaluator(expressionEvalCacheSize, sa.stringTablePurgeLimit)
	if err != nil {
		fatalf("Failed to create IL expression evaluator with cache size %d: %v", expressionEvalCacheSize, err)
	}

	evalForLegacy = ilEvalForLegacy

	var dispatcher mixerRuntime.Dispatcher

	if sa.configStore2URL == "" {
		printf("configStore2URL is not specified, assuming inCluster Kubernetes")
		sa.configStore2URL = "k8s://"
	}

	adapterMap := config.InventoryMap(adapters)
	store2, err := store.NewRegistry2(config.Store2Inventory()...).NewStore2(sa.configStore2URL)
	if err != nil {
		fatalf("Failed to connect to the configuration server. %v", err)
	}
	dispatcher, err = mixerRuntime.New(eval, evaluator.NewTypeChecker(), gp, adapterGP,
		sa.configIdentityAttribute, sa.configDefaultNamespace,
		store2, adapterMap, info,
	)
	if err != nil {
		fatalf("Failed to create runtime dispatcher. %v", err)
	}

	// Legacy Runtime
	repo := template.NewRepository(info)
	store := configStore(sa.configStoreURL, sa.serviceConfigFile, sa.globalConfigFile, printf, fatalf)
	adapterMgr := adapterManager.NewManager(legacyAdapters, aspect.Inventory(), evalForLegacy, gp, adapterGP)
	configManager := config.NewManager(evalForLegacy, evaluator.NewTypeChecker(), adapterMgr.AspectValidatorFinder, adapterMgr.BuilderValidatorFinder, adapters,
		adapterMgr.SupportedKinds,
		repo, store, time.Second*time.Duration(sa.configFetchIntervalSec),
		sa.configIdentityAttribute,
		sa.configIdentityAttributeDomain)

	configAPIServer := config.NewAPI("v1", sa.configAPIPort, evaluator.NewTypeChecker(),
		adapterMgr.AspectValidatorFinder, adapterMgr.BuilderValidatorFinder, adapters,
		adapterMgr.SupportedKinds, store, repo)

	var serverCert *tls.Certificate
	var clientCerts *x509.CertPool

	if sa.serverCertFile != "" && sa.serverKeyFile != "" {
		var sc tls.Certificate
		if sc, err = tls.LoadX509KeyPair(sa.serverCertFile, sa.serverKeyFile); err != nil {
			fatalf("Failed to load server certificate and server key: %v", err)
		}
		serverCert = &sc
	}

	if sa.clientCertFiles != "" {
		clientCerts = x509.NewCertPool()
		for _, clientCertFile := range strings.Split(sa.clientCertFiles, ",") {
			var pem []byte
			if pem, err = ioutil.ReadFile(clientCertFile); err != nil {
				fatalf("Failed to load client certificate: %v", err)
			}
			clientCerts.AppendCertsFromPEM(pem)
		}
	}

	// construct the gRPC options

	var grpcOptions []grpc.ServerOption
	grpcOptions = append(grpcOptions, grpc.MaxConcurrentStreams(uint32(sa.maxConcurrentStreams)))
	grpcOptions = append(grpcOptions, grpc.MaxMsgSize(int(sa.maxMessageSize)))

	if sa.compressedPayload {
		grpcOptions = append(grpcOptions, grpc.RPCCompressor(grpc.NewGZIPCompressor()))
		grpcOptions = append(grpcOptions, grpc.RPCDecompressor(grpc.NewGZIPDecompressor()))
	}

	if serverCert != nil {
		// enable TLS
		tlsConfig := &tls.Config{}
		tlsConfig.Certificates = []tls.Certificate{*serverCert}

		if clientCerts != nil {
			// enable TLS mutual auth
			tlsConfig.ClientCAs = clientCerts
			tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		}
		tlsConfig.BuildNameToCertificate()

		grpcOptions = append(grpcOptions, grpc.Creds(credentials.NewTLS(tlsConfig)))
	}

	var interceptors []grpc.UnaryServerInterceptor

	if len(sa.zipkinURL) > 0 || len(sa.jaegerURL) > 0 || sa.logTraceSpans {
		opts := make([]tracing.Option, 0, 3)
		if len(sa.zipkinURL) > 0 {
			opts = append(opts, tracing.WithZipkinCollector(sa.zipkinURL))
		}
		if len(sa.jaegerURL) > 0 {
			opts = append(opts, tracing.WithJaegerHTTPCollector(sa.jaegerURL))
		}
		if sa.logTraceSpans {
			opts = append(opts, tracing.WithLogger())
		}
		tracer, closer, err := tracing.NewTracer("istio-mixer", opts...)
		if err != nil {
			fatalf("Could not create tracer: %v", err)
		}
		closers = append(closers, closer)
		ot.InitGlobalTracer(tracer)
		interceptors = append(interceptors, otgrpc.OpenTracingServerInterceptor(tracer))
	}

	// setup server prometheus monitoring (as final interceptor in chain)
	interceptors = append(interceptors, grpc_prometheus.UnaryServerInterceptor)
	grpc_prometheus.EnableHandlingTimeHistogram()
	grpcOptions = append(grpcOptions, grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(interceptors...)))

	configManager.Register(adapterMgr)
	configManager.Register(ilEvalForLegacy)

	configManager.Start()

	printf("Starting Config API server on port %v", sa.configAPIPort)
	go configAPIServer.Run()

	var monitoringListener net.Listener
	// get the network stuff setup
	if monitoringListener, err = net.Listen("tcp", fmt.Sprintf(":%d", sa.monitoringPort)); err != nil {
		fatalf("Unable to listen on socket: %v", err)
	}

	// NOTE: this is a temporary solution for provide bare-bones debug functionality
	// for mixer. a full design / implementation of self-monitoring and reporting
	// is coming. that design will include proper coverage of statusz/healthz type
	// functionality, in addition to how mixer reports its own metrics.
	http.Handle(metricsPath, promhttp.Handler())
	http.HandleFunc(versionPath, func(out http.ResponseWriter, req *http.Request) {
		if _, verErr := out.Write([]byte(version.Info.String())); verErr != nil {
			printf("error printing version info: %v", verErr)
		}
	})
	monitoring := &http.Server{Addr: fmt.Sprintf(":%d", sa.monitoringPort)}
	printf("Starting self-monitoring on port %d", sa.monitoringPort)
	go func() {
		if monErr := monitoring.Serve(monitoringListener.(*net.TCPListener)); monErr != nil {
			printf("monitoring server error: %v", monErr)
		}
	}()

	// get everything wired up
	gs := grpc.NewServer(grpcOptions...)

	s := api.NewGRPCServer(adapterMgr, dispatcher, gp)
	mixerpb.RegisterMixerServer(gs, s)
	return &ServerContext{GP: gp, AdapterGP: adapterGP, Server: gs, Closers: closers}
}

func runServer(sa *serverArgs, info map[string]template.Info, adapters []adptr.InfoFn, legacyAdapters []adptr.RegisterFn, printf, fatalf shared.FormatFn) {
	printf("Mixer started with\n%s", sa)
	context := setupServer(sa, info, adapters, legacyAdapters, printf, fatalf)
	for _, c := range context.Closers {
		defer c.Close()
	}

	printf("Istio Mixer: %s", version.Info)
	printf("Starting gRPC server on port %v", sa.port)

	var err error
	var listener net.Listener
	// get the network stuff setup
	if listener, err = net.Listen("tcp", fmt.Sprintf(":%d", sa.port)); err != nil {
		fatalf("Unable to listen on socket: %v", err)
	}

	if err = context.Server.Serve(listener); err != nil {
		fatalf("Failed serving gRPC server: %v", err)
	}
}
