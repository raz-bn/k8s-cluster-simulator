// Copyright 2019 Preferred Networks, Inc.
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
	"context"

	"strings"

	"github.com/pfnet-research/k8s-cluster-simulator/pkg/scheduler"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/scheduler/api"
	kutil "k8s.io/kubernetes/pkg/scheduler/util"
)

func filterExtender(args api.ExtenderArgs) api.ExtenderFilterResult {
	// Filters out no nodes.
	return api.ExtenderFilterResult{
		Nodes:       &v1.NodeList{},
		NodeNames:   args.NodeNames,
		FailedNodes: api.FailedNodesMap{},
		Error:       "",
	}
}

const parralel = true

func prioritizeExtender(args api.ExtenderArgs) api.HostPriorityList {
	// Ranks all nodes equally.
	priorities := make(api.HostPriorityList, 0, len(*args.NodeNames))
	for _, name := range *args.NodeNames {
		priorities = append(priorities, api.HostPriority{Host: name, Score: 1})
	}
	return priorities
}

func min(a, b int) int {
	if a > b {
		return b
	}
	return a
}

func prioritizeLowUsageNode(args api.ExtenderArgs) api.HostPriorityList {
	priorities := make(api.HostPriorityList, len(*args.NodeNames))
	if parralel {
		ctx, _ := context.WithCancel(context.Background())
		// Run predicate plugins in parallel along nodes.
		workqueue.ParallelizeUntil(ctx, workerNum, int(len(*args.NodeNames)), func(i int) {
			name := (*args.NodeNames)[i]
			if _, ok := scheduler.NodeMetricsCache[name]; ok {
				usage := scheduler.NodeMetricsCache[name].Usage
				capacity := scheduler.NodeMetricsCache[name].Allocatable
				cScore := int(api.MaxPriority * (capacity.MilliCPU - usage.MilliCPU) / capacity.MilliCPU)
				mScore := int(api.MaxPriority * (capacity.Memory - usage.Memory) / capacity.Memory)
				priorities[i] = api.HostPriority{Host: name, Score: min(cScore, mScore)}
			} else {
				priorities[i] = api.HostPriority{Host: name, Score: api.MaxPriority}
			}
		})
	} else {
		for i, name := range *args.NodeNames {
			if _, ok := scheduler.NodeMetricsCache[name]; ok {
				usage := scheduler.NodeMetricsCache[name].Usage
				capacity := scheduler.NodeMetricsCache[name].Allocatable
				cScore := int(api.MaxPriority * (capacity.MilliCPU - usage.MilliCPU) / capacity.MilliCPU)
				mScore := int(api.MaxPriority * (capacity.Memory - usage.Memory) / capacity.Memory)
				priorities[i] = api.HostPriority{Host: name, Score: min(cScore, mScore)}
			} else {
				priorities[i] = api.HostPriority{Host: name, Score: api.MaxPriority}
			}
		}
	}
	return priorities
}

func jobName(pod *v1.Pod) string {
	strs := strings.Split(pod.Name, "-")
	if len(strs) < 2 {
		return pod.Name
	}

	return strs[1]
}

func filterFitResource(args api.ExtenderArgs) api.ExtenderFilterResult {
	nodeList := v1.NodeList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "NodeList",
			APIVersion: "v1",
		},
		// ListMeta: metav1.ListMeta{},
		Items: make([]v1.Node, 0, len(*args.NodeNames)),
	}
	nodeNames := make([]string, 0, len(*args.NodeNames))
	failedNodesMap := make(map[string]string)
	request := kutil.GetResourceRequest(args.Pod)
	if parralel {
		ctx, _ := context.WithCancel(context.Background())
		// Run predicate plugins in parallel along nodes.
		filtered := make([]string, len(*args.NodeNames))
		workqueue.ParallelizeUntil(ctx, workerNum, int(len(*args.NodeNames)), func(i int) {
			name := (*args.NodeNames)[i]
			if _, ok := scheduler.NodeMetricsCache[name]; ok {
				usage := scheduler.NodeMetricsCache[name].Usage
				capacity := scheduler.NodeMetricsCache[name].Allocatable
				if (capacity.MilliCPU-usage.MilliCPU-request.MilliCPU) < 0 || (capacity.Memory-usage.Memory-request.Memory) < 0 {
					filtered[i] = ""
				} else {
					filtered[i] = name
				}
			} else {
				filtered[i] = name
			}
		})
		for _, name := range filtered {
			if name != "" {
				nodeNames = append(nodeNames, name)
			} else {
				failedNodesMap[name] = "This node's usage is too high"
			}
		}
	} else {
		request := kutil.GetResourceRequest(args.Pod)
		for _, name := range *args.NodeNames {
			if _, ok := scheduler.NodeMetricsCache[name]; ok {
				usage := scheduler.NodeMetricsCache[name].Usage
				capacity := scheduler.NodeMetricsCache[name].Allocatable
				if (capacity.MilliCPU-usage.MilliCPU-request.MilliCPU) < 0 || (capacity.Memory-usage.Memory-request.Memory) < 0 {
					failedNodesMap[name] = "This node's usage is too high"
				} else {
					nodeNames = append(nodeNames, name)
				}
			} else {
				nodeNames = append(nodeNames, name)
			}
		}

	}
	return api.ExtenderFilterResult{
		Nodes:       &nodeList,
		NodeNames:   &nodeNames,
		FailedNodes: failedNodesMap,
		Error:       "",
	}
}
