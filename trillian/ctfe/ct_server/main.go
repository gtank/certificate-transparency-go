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

// The ct_server binary runs the CT personality.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/etcd/clientv3"
	etcdnaming "github.com/coreos/etcd/clientv3/naming"
	"github.com/golang/glog"
	"github.com/google/certificate-transparency-go/trillian/ctfe"
	"github.com/google/certificate-transparency-go/trillian/util"
	"github.com/google/trillian"
	"github.com/google/trillian/monitoring/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/naming"
)

// Global flags that affect all log instances.
var (
	httpEndpoint      = flag.String("http_endpoint", "localhost:6962", "Endpoint for HTTP (host:port)")
	rpcBackendFlag    = flag.String("log_rpc_server", "localhost:8090", "Backend specification; comma-separated list or etcd service name (if --etcd_servers specified)")
	rpcDeadlineFlag   = flag.Duration("rpc_deadline", time.Second*10, "Deadline for backend RPC requests")
	logConfigFlag     = flag.String("log_config", "", "File holding log config in JSON")
	maxGetEntriesFlag = flag.Int64("maxGetEntriesAllowed", 0, "Max number of entries we allow in a get-entries request (default 50)")
	etcdServers       = flag.String("etcd_servers", "", "A comma-separated list of etcd servers")
	etcdHTTPService   = flag.String("etcd_http_service", "trillian-ctfe-http", "Service name to announce our HTTP endpoint under")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	if *maxGetEntriesFlag > 0 {
		ctfe.MaxGetEntriesAllowed = *maxGetEntriesFlag
	}

	// Get log config from file before we start.
	cfg, err := ctfe.LogConfigFromFile(*logConfigFlag)
	if err != nil {
		glog.Exitf("Failed to read log config: %v", err)
	}

	glog.CopyStandardLogTo("WARNING")
	glog.Info("**** CT HTTP Server Starting ****")

	// TODO(Martin2112): Support TLS and other stuff for RPC client and http server, this is just to
	// get started. Uses a blocking connection so we don't start serving before we're connected
	// to backend.
	var res naming.Resolver
	if len(*etcdServers) > 0 {
		// Use etcd to provide endpoint resolution.
		cfg := clientv3.Config{Endpoints: strings.Split(*etcdServers, ","), DialTimeout: 5 * time.Second}
		client, err := clientv3.New(cfg)
		if err != nil {
			glog.Exitf("Failed to connect to etcd at %v: %v", *etcdServers, err)
		}
		etcdRes := &etcdnaming.GRPCResolver{Client: client}
		res = etcdRes

		// Also announce ourselves.
		update := naming.Update{Op: naming.Add, Addr: *httpEndpoint}
		etcdRes.Update(ctx, *etcdHTTPService, update)
		glog.Infof("Announcing our presence in %v with %+v", *etcdHTTPService, update)

		bye := naming.Update{Op: naming.Delete, Addr: *httpEndpoint}
		defer func() {
			glog.Infof("Removing our presence in %v with %+v", *etcdHTTPService, update)
			etcdRes.Update(ctx, *etcdHTTPService, bye)
		}()
	} else {
		// Use a fixed endpoint resolution that just returns the addresses configured on the command line.
		res = util.FixedBackendResolver{}
	}
	bal := grpc.RoundRobin(res)

	// If rpcBackendFlag points to a SRV record, resolve it now.
	// Assume it's a SRV if there's no port specified.
	if strings.Index(*rpcBackendFlag, ":") == -1 {
		// Expected format is _service._proto.the.name.can.be.long
		parts := strings.SplitN(*rpcBackendFlag, ".", 3)
		if len(parts) != 3 {
			glog.Exitf("Invalid SRV backend flag: %v", *rpcBackendFlag)
		}
		_, srvRecords, err := net.LookupSRV(parts[0], parts[1], parts[2])
		if err != nil {
			glog.Exitf("Failed SRV lookup for backend: %v", err)
		}
		*rpcBackendFlag = fmt.Sprintf("%s:%d", srvRecords[0].Target, srvRecords[0].Port)
	}

	conn, err := grpc.Dial(*rpcBackendFlag, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithBalancer(bal))
	if err != nil {
		glog.Exitf("Could not connect to rpc server: %v", err)
	}
	defer conn.Close()
	client := trillian.NewTrillianLogClient(conn)

	for _, c := range cfg {
		handlers, err := c.SetUpInstance(client, *rpcDeadlineFlag, prometheus.MetricFactory{})
		if err != nil {
			glog.Exitf("Failed to set up log instance for %+v: %v", cfg, err)
		}
		for path, handler := range *handlers {
			http.Handle(path, handler)
		}
	}
	http.Handle("/metrics", promhttp.Handler())

	// Bring up the HTTP server and serve until we get a signal not to.
	go awaitSignal(func() {
		os.Exit(1)
	})
	server := http.Server{Addr: *httpEndpoint, Handler: nil}
	err = server.ListenAndServe()
	glog.Warningf("Server exited: %v", err)
	glog.Flush()
}

// awaitSignal waits for standard termination signals, then runs the given
// function; it should be run as a separate goroutine.
func awaitSignal(doneFn func()) {
	// Arrange notification for the standard set of signals used to terminate a server
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// Now block main and wait for a signal
	sig := <-sigs
	glog.Warningf("Signal received: %v", sig)
	glog.Flush()

	doneFn()
}
