/*
 *
 *  * Copyright 2021 KubeClipper Authors.
 *  *
 *  * Licensed under the Apache License, Version 2.0 (the "License");
 *  * you may not use this file except in compliance with the License.
 *  * You may obtain a copy of the License at
 *  *
 *  *     http://www.apache.org/licenses/LICENSE-2.0
 *  *
 *  * Unless required by applicable law or agreed to in writing, software
 *  * distributed under the License is distributed on an "AS IS" BASIS,
 *  * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  * See the License for the specific language governing permissions and
 *  * limitations under the License.
 *
 */

package component

import (
	"context"
)

type (
	extraKey     struct{}
	metaKey      struct{}
	operationKey struct{}
	stepKey      struct{}
	oplogKey     struct{}
	retryKey     struct{}
	repoMirror   struct{}
)

type ExtraMetadata struct {
	// master, worker node info
	// Offline 代表是在线还是离线安装
	// LocalRegistry:
	//    在线安装时可以填其他地址，默认是 docker.io
	//    离线安装时可以填镜像来源，不填则使用 http 分发方式
	Masters       NodeList
	Workers       NodeList
	Offline       bool
	LocalRegistry string
	CRI           string
	ClusterName   string
	KubeVersion   string
	OperationType string
}

type Node struct {
	ID       string
	IPv4     string
	Region   string
	Hostname string
	Role     string
	Disable  bool
}

type NodeList []Node

func (l NodeList) GetNodeIDs() (nodes []string) {
	for _, node := range l {
		nodes = append(nodes, node.ID)
	}
	return
}

func (e ExtraMetadata) GetAllNodeIDs() []string {
	var nodes []string
	nodes = append(nodes, e.GetMasterNodeIDs()...)
	nodes = append(nodes, e.GetWorkerNodeIDs()...)
	return nodes
}

func (e ExtraMetadata) GetAllNodes() (nodes NodeList) {
	nodes = append(nodes, e.Masters...)
	nodes = append(nodes, e.Workers...)
	return nodes
}

func (e ExtraMetadata) GetMasterHostname(id string) string {
	for _, node := range e.Masters {
		if node.ID == id {
			return node.Hostname
		}
	}
	return ""
}

func (e ExtraMetadata) GetWorkerHostname(id string) string {
	for _, node := range e.Workers {
		if node.ID == id {
			return node.Hostname
		}
	}
	return ""
}

func (e ExtraMetadata) GetMasterNodeIDs() []string {
	var nodes []string
	for _, node := range e.Masters {
		nodes = append(nodes, node.ID)
	}
	return nodes
}

func (e ExtraMetadata) GetWorkerNodeIDs() []string {
	var nodes []string
	for _, node := range e.Workers {
		nodes = append(nodes, node.ID)
	}
	return nodes
}

func (e ExtraMetadata) GetMasterNodeIP() map[string]string {
	nodes := make(map[string]string)
	for _, node := range e.Masters {
		nodes[node.ID] = node.IPv4
	}
	return nodes
}

func (e ExtraMetadata) GetWorkerNodeIP() map[string]string {
	nodes := make(map[string]string)
	for _, node := range e.Workers {
		nodes[node.ID] = node.IPv4
	}
	return nodes
}

func WithExtraData(ctx context.Context, data []byte) context.Context {
	return context.WithValue(ctx, extraKey{}, data)
}

func GetExtraData(ctx context.Context) []byte {
	if v := ctx.Value(extraKey{}); v != nil {
		return v.([]byte)
	}
	return nil
}

func WithExtraMetadata(ctx context.Context, metadata ExtraMetadata) context.Context {
	return context.WithValue(ctx, metaKey{}, metadata)
}

func GetExtraMetadata(ctx context.Context) ExtraMetadata {
	if v := ctx.Value(metaKey{}); v != nil {
		return v.(ExtraMetadata)
	}
	return ExtraMetadata{}
}

func WithOperationID(ctx context.Context, opID string) context.Context {
	return context.WithValue(ctx, operationKey{}, opID)
}

func WithStepID(ctx context.Context, stepID string) context.Context {
	return context.WithValue(ctx, stepKey{}, stepID)
}

func GetOperationID(ctx context.Context) string {
	if v := ctx.Value(operationKey{}); v != nil {
		return v.(string)
	}
	return ""
}

func GetStepID(ctx context.Context) string {
	if v := ctx.Value(stepKey{}); v != nil {
		return v.(string)
	}
	return ""
}

func WithOplog(ctx context.Context, ol OperationLogFile) context.Context {
	return context.WithValue(ctx, oplogKey{}, ol)
}

func GetOplog(ctx context.Context) OperationLogFile {
	if v := ctx.Value(oplogKey{}); v != nil {
		return v.(OperationLogFile)
	}
	return nil
}

func WithRetry(ctx context.Context, retry bool) context.Context {
	return context.WithValue(ctx, retryKey{}, retry)
}

func GetRetry(ctx context.Context) bool {
	if v := ctx.Value(retryKey{}); v != nil {
		return v.(bool)
	}
	return false
}

func WithRepoMirror(ctx context.Context, mirror string) context.Context {
	return context.WithValue(ctx, repoMirror{}, mirror)
}

func GetRepoMirror(ctx context.Context) string {
	if v := ctx.Value(repoMirror{}); v != nil {
		return v.(string)
	}
	return ""
}
