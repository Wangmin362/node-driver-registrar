/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"google.golang.org/grpc"

	"github.com/kubernetes-csi/node-driver-registrar/pkg/util"
	"k8s.io/klog/v2"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

func nodeRegister(csiDriverName, httpEndpoint string) {
	// When kubeletRegistrationPath is specified then driver-registrar ONLY acts
	// as gRPC server which replies to registration requests initiated by kubelet's
	// plugins watcher infrastructure. Node labeling is done by kubelet's csi code.
	// TODO 注册服务实现了Kubelet RegistrationServer接口，应该是用来注册插件的
	registrar := newRegistrationServer(csiDriverName, *kubeletRegistrationPath, supportedVersions)
	// 拼接socket完整路径：<pluginRegistrationPath>/<CSIPluginName>-reg.sock
	// 譬如：/var/lib/kubelet/plugins_registry/nfs.csi.k8s.io-reg.sock
	socketPath := buildSocketPath(csiDriverName)
	// 如果当前的Socket文件存在，就移除Socket文件，注意这个Socket文件并不是CSI插件Socket文件
	if err := util.CleanupSocketFile(socketPath); err != nil {
		klog.Errorf("%+v", err)
		os.Exit(1)
	}

	var oldmask int
	if runtime.GOOS == "linux" {
		// Default to only user accessible socket, caller can open up later if desired
		oldmask, _ = util.Umask(0077)
	}

	klog.Infof("Starting Registration Server at: %s\n", socketPath)
	// 监听socket
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		klog.Errorf("failed to listen on socket: %s with error: %+v", socketPath, err)
		os.Exit(1)
	}
	if runtime.GOOS == "linux" {
		util.Umask(oldmask)
	}
	klog.Infof("Registration Server started at: %s\n", socketPath)
	grpcServer := grpc.NewServer()

	// Before registering node-driver-registrar with the kubelet ensure that the lockfile doesn't exist
	// a lockfile may exist because the container was forcefully shutdown
	util.CleanupFile(registrationProbePath)

	// Registers kubelet plugin watcher api.
	// 注册服务
	registerapi.RegisterRegistrationServer(grpcServer, registrar)

	// 提供健康检测断电以及pprof检测端点
	go httpServer(socketPath, httpEndpoint)
	// 如果node-driver-register服务被停止，就需要删除socket文件。
	go removeRegSocket(csiDriverName)
	// 启动RegistrationServer,Kubelet可以通过这个socket文件调用其约定的方法
	if err := grpcServer.Serve(lis); err != nil {
		klog.Errorf("Registration Server stopped serving: %v", err)
		os.Exit(1)
	}

	// clean the file on graceful shutdown
	util.CleanupFile(registrationProbePath)
	// If gRPC server is gracefully shutdown, cleanup and exit
	os.Exit(0)
}

func buildSocketPath(csiDriverName string) string {
	return fmt.Sprintf("%s/%s-reg.sock", *pluginRegistrationPath, csiDriverName)
}

func httpServer(socketPath string, httpEndpoint string) {
	if httpEndpoint == "" {
		klog.Infof("Skipping HTTP server because endpoint is set to: %q", httpEndpoint)
		return
	}
	klog.Infof("Starting HTTP server at endpoint: %v\n", httpEndpoint)

	// Prepare http endpoint for healthz + profiling (if enabled)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
		socketExists, err := util.DoesSocketExist(socketPath)
		if err == nil && socketExists {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`ok`))
			klog.V(5).Infof("health check succeeded")
		} else if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			klog.Errorf("health check failed: %+v", err)
		} else if !socketExists {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("registration socket does not exist"))
			klog.Errorf("health check failed, registration socket does not exist")
		}
	})

	if *enableProfile {
		klog.InfoS("Starting profiling", "endpoint", httpEndpoint)

		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	klog.Fatal(http.ListenAndServe(httpEndpoint, mux))
}

func removeRegSocket(csiDriverName string) {
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM)
	<-sigc
	socketPath := buildSocketPath(csiDriverName)
	err := os.Remove(socketPath)
	if err != nil && !os.IsNotExist(err) {
		klog.Errorf("failed to remove socket: %s with error: %+v", socketPath, err)
		os.Exit(1)
	}
	os.Exit(0)
}
