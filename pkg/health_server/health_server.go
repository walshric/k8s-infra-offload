// Copyright (c) 2022 Intel Corporation.  All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License")
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

package healthserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ipdk-io/k8s-infra-offload/pkg/types"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"gopkg.in/tomb.v2"
)

var (
	grpcDial = grpc.Dial
)

type httpHealthServer interface {
	Shutdown(ctx context.Context) error
	ListenAndServe() error
}

type healtServer struct {
	log *logrus.Entry
	srv httpHealthServer
}

func getCheck(hs *healtServer) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		hs.log.Infof("Receive request %v", r)
		// check status of infra manager
		if ok := hs.checkInfraManagerLiveness(); !ok {
			hs.log.Infof("infra manager report failure")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// check status of cni grpc server status
		if ok := hs.checkCniServerLiveness(); !ok {
			hs.log.Infof("CNI reports failure")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// check status of services server
		if ok := hs.checkServicesServerStatus(); !ok {
			w.WriteHeader(http.StatusInternalServerError)
			hs.log.Infof("services server reports failure")
			return
		}

		w.WriteHeader(http.StatusOK)
	}
}

func NewHealthCheckServer(l *logrus.Entry) (types.Server, error) {
	hs := &healtServer{
		log: l,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/check", getCheck(hs))
	hs.srv = &http.Server{
		Addr:    ":" + types.DefaultHealthServerPort,
		Handler: mux,
	}
	return hs, nil
}

func (s *healtServer) checkGrpcServerStatus(target string) bool {
	s.log.Infof("Checking gRCP service at %s", target)
	conn, err := grpcDial(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer func() {
		if conn == nil {
			return
		}
		if err := conn.Close(); err != nil {
			s.log.WithError(err).Error("failed to close connection")
		}
	}()
	if err != nil {
		s.log.WithError(err).Error("Failed to dial gRPC health server")
		return false
	}
	resp, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{Service: ""})
	if err != nil {
		s.log.WithError(err).Error("Failed to check gRPC health")
		return false
	}
	s.log.Infof("Status of gRPC service at %s is: %s", target, resp.GetStatus().String())
	return resp.Status == healthpb.HealthCheckResponse_SERVING
}

func (s *healtServer) checkInfraManagerLiveness() bool {
	managerAddr := fmt.Sprintf("%s:%s", types.InfraManagerAddr, types.InfraManagerPort)
	return s.checkGrpcServerStatus(managerAddr)
}

func (s *healtServer) checkCniServerLiveness() bool {
	// TODO change this to UDS when grpc start working using it
	agentAddr := fmt.Sprintf("%s:%s", types.InfraAgentAddr, types.InfraAgentPort)
	return s.checkGrpcServerStatus(agentAddr)
}

func (s *healtServer) checkServicesServerStatus() bool {
	s.log.Infof("Status of services server: %s", types.ServiceServerStatus)
	return types.ServiceServerStatus == types.ServerStatusOK
}

func (s *healtServer) GetName() string {
	return "health-server"
}

func (s *healtServer) StopServer() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		s.log.WithError(err).Error("Failed to close healt server")
	}
}

func (s *healtServer) serve() error {
	err := s.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		s.log.Infof("health server is closed")
	} else if err != nil {
		s.log.WithError(err).Error("Error listening for health server")
		return err
	}
	return nil
}

func (s *healtServer) Start(t *tomb.Tomb) error {
	go func() {
		if err := s.serve(); err != nil {
			s.log.Warnf("Error when serving %s error %v", s.GetName(), err)
		}
	}()
	<-t.Dying()
	// stop server
	s.StopServer()
	return nil
}
