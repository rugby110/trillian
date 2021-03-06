// Copyright 2016 Google Inc. All Rights Reserved.
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

package main

import (
	"flag"
	_ "net/http/pprof"

	_ "github.com/go-sql-driver/mysql"              // Load MySQL driver
	_ "github.com/google/trillian/merkle/coniks"    // Make hashers available
	_ "github.com/google/trillian/merkle/maphasher" // Make hashers available

	"github.com/golang/glog"
	"github.com/google/trillian"
	"github.com/google/trillian/cmd"
	"github.com/google/trillian/crypto/keys"
	"github.com/google/trillian/extension"
	"github.com/google/trillian/monitoring"
	"github.com/google/trillian/monitoring/prometheus"
	mysqlq "github.com/google/trillian/quota/mysql"
	"github.com/google/trillian/server"
	"github.com/google/trillian/server/interceptor"
	"github.com/google/trillian/storage/mysql"
	"github.com/google/trillian/util"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

var (
	mySQLURI           = flag.String("mysql_uri", "test:zaphod@tcp(127.0.0.1:3306)/test", "Connection URI for MySQL database")
	rpcEndpoint        = flag.String("rpc_endpoint", "localhost:8090", "Endpoint for RPC requests (host:port)")
	httpEndpoint       = flag.String("http_endpoint", "localhost:8091", "Endpoint for HTTP metrics and REST requests on (host:port, empty means disabled)")
	maxUnsequencedRows = flag.Int("max_unsequenced_rows", mysqlq.DefaultMaxUnsequenced, "Max number of unsequenced rows before rate limiting kicks in")

	configFile = flag.String("config", "", "Config file containing flags, file contents can be overridden by command line flags")
)

func main() {
	flag.Parse()

	if *configFile != "" {
		if err := cmd.ParseFlagFile(*configFile); err != nil {
			glog.Exitf("Failed to load flags from config file %q: %s", *configFile, err)
		}
	}

	db, err := mysql.OpenDB(*mySQLURI)
	if err != nil {
		glog.Exitf("Failed to open database: %v", err)
	}
	// No defer: database ownership is delegated to server.Main

	registry := extension.Registry{
		AdminStorage:  mysql.NewAdminStorage(db),
		SignerFactory: &keys.DefaultSignerFactory{},
		MapStorage:    mysql.NewMapStorage(db),
		QuotaManager:  &mysqlq.QuotaManager{DB: db, MaxUnsequencedRows: *maxUnsequencedRows},
		MetricFactory: prometheus.MetricFactory{},
	}

	ts := util.SystemTimeSource{}
	stats := monitoring.NewRPCStatsInterceptor(ts, "map", registry.MetricFactory)
	ti := &interceptor.TrillianInterceptor{
		Admin:        registry.AdminStorage,
		QuotaManager: registry.QuotaManager,
	}
	netInterceptor := interceptor.Combine(stats.Interceptor(), interceptor.ErrorWrapper, ti.UnaryInterceptor)
	s := grpc.NewServer(grpc.UnaryInterceptor(netInterceptor))
	// No defer: server ownership is delegated to server.Main

	m := server.Main{
		RPCEndpoint:       *rpcEndpoint,
		HTTPEndpoint:      *httpEndpoint,
		DB:                db,
		Registry:          registry,
		Server:            s,
		RegisterHandlerFn: trillian.RegisterTrillianMapHandlerFromEndpoint,
		RegisterServerFn: func(s *grpc.Server, registry extension.Registry) error {
			mapServer := server.NewTrillianMapServer(registry)
			if err := mapServer.IsHealthy(); err != nil {
				return err
			}
			trillian.RegisterTrillianMapServer(s, mapServer)
			return err
		},
	}

	ctx := context.Background()
	if err := m.Run(ctx); err != nil {
		glog.Exitf("Server exited with error: %v", err)
	}
}
