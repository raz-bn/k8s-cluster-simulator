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

package scheduler

import (
	"errors"
	"fmt"
	"math"

	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/algorithm"
	"k8s.io/kubernetes/pkg/scheduler/algorithm/predicates"
	"k8s.io/kubernetes/pkg/scheduler/algorithm/priorities"
	"k8s.io/kubernetes/pkg/scheduler/api"
	"k8s.io/kubernetes/pkg/scheduler/core"
	"k8s.io/kubernetes/pkg/scheduler/nodeinfo"
	kutil "k8s.io/kubernetes/pkg/scheduler/util"

	"github.com/pfnet-research/k8s-cluster-simulator/pkg/clock"
	l "github.com/pfnet-research/k8s-cluster-simulator/pkg/log"
	"github.com/pfnet-research/k8s-cluster-simulator/pkg/metrics"
	"github.com/pfnet-research/k8s-cluster-simulator/pkg/node"
	"github.com/pfnet-research/k8s-cluster-simulator/pkg/queue"
	"github.com/pfnet-research/k8s-cluster-simulator/pkg/util"
)

// ProposedScheduler makes scheduling decision for each given pod in the one-by-one manner and pick the busiest pod first.
type ProposedScheduler struct {
	extenders    []Extender
	predicates   map[string]predicates.FitPredicate
	prioritizers []priorities.PriorityConfig

	lastNodeIndex     uint64
	preemptionEnabled bool
	failQueue         *queue.FIFOQueue
}

// NewProposedScheduler creates a new ProposedScheduler.
func NewProposedScheduler(preeptionEnabled bool) ProposedScheduler {
	return ProposedScheduler{
		predicates:        map[string]predicates.FitPredicate{},
		preemptionEnabled: preeptionEnabled,
		failQueue:         queue.NewFIFOQueue(),
	}
}

// AddExtender adds an extender to this ProposedScheduler.
func (sched *ProposedScheduler) AddExtender(extender Extender) {
	sched.extenders = append(sched.extenders, extender)
}

// AddPredicate adds a predicate plugin to this ProposedScheduler.
func (sched *ProposedScheduler) AddPredicate(name string, predicate predicates.FitPredicate) {
	sched.predicates[name] = predicate
}

// AddPrioritizer adds a prioritizer plugin to this ProposedScheduler.
func (sched *ProposedScheduler) AddPrioritizer(prioritizer priorities.PriorityConfig) {
	sched.prioritizers = append(sched.prioritizers, prioritizer)
}

// Schedule implements Scheduler interface.
// Schedules pods in one-by-one manner by using registered extenders and plugins.
func (sched *ProposedScheduler) Schedule(
	clock clock.Clock,
	pendingPods queue.PodQueue,
	nodeLister algorithm.NodeLister,
	nodeInfoMap map[string]*nodeinfo.NodeInfo) ([]Event, error) {

	// update NodesOverSubFactors
	for nodeName, _ := range nodeInfoMap {
		nodesMet := GlobalMetrics[metrics.NodesMetricsKey].(map[string]node.Metrics)
		usage := nodesMet[nodeName].TotalResourceUsage
		allocatable := nodesMet[nodeName].Allocatable
		request := nodesMet[nodeName].TotalResourceRequest
		factor := float64(0.9)
		if !util.ResourceListLEWithFactor(request, allocatable, factor) && util.ResourceListLEWithFactor(usage, allocatable, factor) {
			// if util.ResourceListLEWithFactor(usage, allocatable, factor) {
			maxOverSub := 2.0
			oversub := math.Min(predicates.NodesOverSubFactors[nodeName]+0.1, maxOverSub)
			// oversub := 2.0s
			predicates.NodesOverSubFactors[nodeName] = oversub
		} else {
			predicates.NodesOverSubFactors[nodeName] = 1.0
		}
	}

	results := []Event{}
	for {
		// For each pod popped from the front of the queue, ...
		pod, err := pendingPods.Front() // not pop a pod here; it may fail to any node
		if err != nil {
			if err == queue.ErrEmptyQueue {
				break
			} else {
				return []Event{}, errors.New("Unexpected error raised by Queueu.Pop()")
			}
		}

		log.L.Tracef("Trying to schedule pod %v", pod)

		podKey, err := util.PodKey(pod)
		if err != nil {
			return []Event{}, err
		}
		log.L.Debugf("Trying to schedule pod %s", podKey)

		// ... try to bind the pod to a node.
		result, err := sched.scheduleOne(pod, nodeLister, nodeInfoMap, pendingPods)

		if err != nil {
			if KeepScheduling {
				err = sched.failQueue.Push(pod)
				if err != nil {
					log.L.Errorf("Cannot push pod to failQueue: %v", err)
				}
				pendingPods.Pop()
				if sched.failQueue.Len() > KeepSchedulingTimeout {
					break
				}
			} else {
				updatePodStatusSchedulingFailure(clock, pod, err)

				// If failed to select a node that can accommodate the pod, ...
				if fitError, ok := err.(*core.FitError); ok {
					log.L.Tracef("Pod %v does not fit in any node", pod)
					log.L.Debugf("Pod %s does not fit in any node", podKey)

					// ... and preemption is enabled, ...
					if sched.preemptionEnabled {
						log.L.Debug("Trying preemption")

						// ... try to preempt other low-priority pods.
						delEvents, err := sched.preempt(pod, pendingPods, nodeLister, nodeInfoMap, fitError)
						if err != nil {
							return []Event{}, err
						}

						// Delete the victim pods.
						results = append(results, delEvents...)
					}

					// Else, stop the scheduling process at this clock.
					break
				} else {
					return []Event{}, nil
				}
			}
		} else {
			// If found a node that can accommodate the pod, ...
			log.L.Debugf("Selected node %s", result.SuggestedHost)

			pod, _ = pendingPods.Pop()
			updatePodStatusSchedulingSucceess(clock, pod)
			if err := pendingPods.RemoveNominatedNode(pod); err != nil {
				return []Event{}, err
			}

			nodeInfo, ok := nodeInfoMap[result.SuggestedHost]
			if !ok {
				return []Event{}, fmt.Errorf("No node named %s", result.SuggestedHost)
			}
			nodeInfo.AddPod(pod)

			// ... then bind it to the node.
			results = append(results, &BindEvent{Pod: pod, ScheduleResult: result})
		}
	}

	if KeepScheduling {
		for {
			// For each pod popped from the front of the queue, ...
			pod, err := sched.failQueue.Pop()
			if err != nil {
				if err == queue.ErrEmptyQueue {
					break
				} else {
					return []Event{}, errors.New("Unexpected error raised by Queueu.Pop()")
				}
			}
			pendingPods.Push(pod)
		}
	}

	return results, nil
}

var _ = Scheduler(&ProposedScheduler{})

// scheduleOne makes scheduling decision for the given pod and nodes.
// Returns core.ErrNoNodesAvailable if nodeLister lists zero nodes, or core.FitError if the given
// pod does not fit in any nodes.
func (sched *ProposedScheduler) scheduleOne(
	pod *v1.Pod,
	nodeLister algorithm.NodeLister,
	nodeInfoMap map[string]*nodeinfo.NodeInfo,
	podQueue queue.PodQueue) (core.ScheduleResult, error) {

	result := core.ScheduleResult{}
	nodes, err := nodeLister.List()

	if err != nil {
		return result, err
	}

	if len(nodes) == 0 {
		return result, core.ErrNoNodesAvailable
	}

	// Filter out nodes that cannot accommodate the pod.
	nodesFiltered, failedPredicateMap, err := sched.filter(pod, nodes, nodeInfoMap, podQueue)
	if err != nil {
		return result, err
	}

	switch len(nodesFiltered) {
	case 0: // The pod doesn't fit in any node.
		return result, &core.FitError{
			Pod:              pod,
			NumAllNodes:      len(nodes),
			FailedPredicates: failedPredicateMap,
		}
	case 1: // Only one node can accommodate the pod; just return it.
		return core.ScheduleResult{
			SuggestedHost:  nodesFiltered[0].Name,
			EvaluatedNodes: 1 + len(failedPredicateMap),
			FeasibleNodes:  1,
		}, nil
	}

	// Prioritize nodes that have passed the filtering phase.
	prios, err := sched.prioritize(pod, nodesFiltered, nodeInfoMap, podQueue)
	if err != nil {
		return result, err
	}

	// Select the node that has the highest score.
	host, err := sched.selectHost(prios)

	return core.ScheduleResult{
		SuggestedHost:  host,
		EvaluatedNodes: len(nodesFiltered) + len(failedPredicateMap),
		FeasibleNodes:  len(nodesFiltered),
	}, err
}

// selectHost takes a prioritized list of nodes and then picks one
// in a round-robin manner from the nodes that had the highest score.
func (sched *ProposedScheduler) selectHost(priorities api.HostPriorityList) (string, error) {
	if len(priorities) == 0 {
		return "", errors.New("Empty priorities")
	}

	maxScores := findMaxScores(priorities)
	// idx := int(sched.lastNodeIndex % uint64(len(maxScores)))
	// sched.lastNodeIndex++

	// return priorities[maxScores[idx]].Host, nil
	// TanLe: Fix the issue for best-fit: do not allow round-robin
	idx := len(maxScores) - 1
	return priorities[maxScores[idx]].Host, nil
}

func (sched *ProposedScheduler) filter(
	pod *v1.Pod,
	nodes []*v1.Node,
	nodeInfoMap map[string]*nodeinfo.NodeInfo,
	podQueue queue.PodQueue,
) ([]*v1.Node, core.FailedPredicateMap, error) {

	if l.IsDebugEnabled() {
		nodeNames := make([]string, 0, len(nodes))
		for _, node := range nodes {
			nodeNames = append(nodeNames, node.Name)
		}
		log.L.Debugf("Filtering nodes %v", nodeNames)
	}

	// In-process plugins
	filtered, failedPredicateMap, err := filterWithPlugins(pod, sched.predicates, nodes, nodeInfoMap, podQueue)
	if err != nil {
		return []*v1.Node{}, core.FailedPredicateMap{}, err
	}

	if l.IsDebugEnabled() {
		nodeNames := make([]string, 0, len(filtered))
		for _, node := range filtered {
			nodeNames = append(nodeNames, node.Name)
		}
		log.L.Debugf("Plugins filtered nodes %v", nodeNames)
	}

	// Extenders
	if len(filtered) > 0 && len(sched.extenders) > 0 {
		for _, extender := range sched.extenders {
			var err error
			filtered, err = extender.filter(pod, filtered, nodeInfoMap, failedPredicateMap)
			if err != nil {
				return []*v1.Node{}, core.FailedPredicateMap{}, err
			}

			if len(filtered) == 0 {
				break
			}
		}
	}

	if l.IsDebugEnabled() {
		nodeNames := make([]string, 0, len(filtered))
		for _, node := range filtered {
			nodeNames = append(nodeNames, node.Name)
		}
		log.L.Debugf("Filtered nodes %v", nodeNames)
	}

	return filtered, failedPredicateMap, nil
}

func (sched *ProposedScheduler) prioritize(
	pod *v1.Pod,
	filteredNodes []*v1.Node,
	nodeInfoMap map[string]*nodeinfo.NodeInfo,
	podQueue queue.PodQueue) (api.HostPriorityList, error) {

	if l.IsDebugEnabled() {
		nodeNames := make([]string, 0, len(filteredNodes))
		for _, node := range filteredNodes {
			nodeNames = append(nodeNames, node.Name)
		}
		log.L.Debugf("Prioritizing nodes %v", nodeNames)
	}

	// If no priority configs are provided, then the EqualPriority function is applied.
	// This is required to generate the priority list in the required format.
	if len(sched.prioritizers) == 0 && len(sched.extenders) == 0 {
		prioList := make(api.HostPriorityList, 0, len(filteredNodes))

		for _, node := range filteredNodes {
			nodeInfo, ok := nodeInfoMap[node.Name]
			if !ok {
				return api.HostPriorityList{}, fmt.Errorf("No node named %s", node.Name)
			}

			prio, err := core.EqualPriorityMap(pod, &dummyPriorityMetadata{}, nodeInfo)
			if err != nil {
				return api.HostPriorityList{}, err
			}
			prioList = append(prioList, prio)
		}

		return prioList, nil
	}

	// In-process plugins
	prioList, err := prioritizeWithPlugins(pod, sched.prioritizers, filteredNodes, nodeInfoMap, podQueue)
	if err != nil {
		return api.HostPriorityList{}, err
	}

	if l.IsDebugEnabled() {
		nodeNames := make([]string, 0, len(filteredNodes))
		for _, node := range filteredNodes {
			nodeNames = append(nodeNames, node.Name)
		}
		log.L.Debugf("Plugins prioritized nodes %v", nodeNames)
	}

	// Extenders
	if len(sched.extenders) > 0 {
		prioMap := map[string]int{}
		for _, extender := range sched.extenders {
			extender.prioritize(pod, filteredNodes, prioMap)
		}

		for i, prio := range prioList {
			prioList[i].Score += prioMap[prio.Host]
		}
	}

	log.L.Debugf("Prioritized nodes %v", prioList)

	return prioList, nil
}

func (sched *ProposedScheduler) preempt(
	preemptor *v1.Pod,
	podQueue queue.PodQueue,
	nodeLister algorithm.NodeLister,
	nodeInfoMap map[string]*nodeinfo.NodeInfo,
	fitError *core.FitError) ([]Event, error) {

	node, victims, nominatedPodsToClear, err := sched.findPreemption(
		preemptor, podQueue, nodeLister, nodeInfoMap, fitError)
	if err != nil {
		return []Event{}, err
	}

	delEvents := make([]Event, 0, len(victims))
	if node != nil {
		log.L.Tracef("Node %v selected for victim", node)
		log.L.Debugf("Node %s selected for victim", node.Name)

		// Nominate the victim node for the preemptor pod.
		if err := podQueue.UpdateNominatedNode(preemptor, node.Name); err != nil {
			return []Event{}, err
		}

		// Delete the victim pods.
		for _, victim := range victims {
			log.L.Tracef("Pod %v selected for victim", victim)

			if l.IsDebugEnabled() {
				key, err := util.PodKey(victim)
				if err != nil {
					return []Event{}, err
				}
				log.L.Debugf("Pod %s selected for victim", key)
			}

			event := DeleteEvent{PodNamespace: victim.Namespace, PodName: victim.Name, NodeName: node.Name}
			delEvents = append(delEvents, &event)
		}
	}

	// Clear nomination of pods that previously have nomination.
	for _, pod := range nominatedPodsToClear {
		log.L.Tracef("Nomination of pod %v cleared", pod)

		if l.IsDebugEnabled() {
			key, err := util.PodKey(pod)
			if err != nil {
				return []Event{}, err
			}
			log.L.Debugf("Nomination of pod %s cleared", key)
		}

		if err := podQueue.RemoveNominatedNode(pod); err != nil {
			return []Event{}, err
		}
	}

	return delEvents, nil
}

func (sched *ProposedScheduler) findPreemption(
	preemptor *v1.Pod,
	podQueue queue.PodQueue,
	nodeLister algorithm.NodeLister,
	nodeInfoMap map[string]*nodeinfo.NodeInfo,
	fitError *core.FitError,
) (selectedNode *v1.Node, preemptedPods []*v1.Pod, cleanupNominatedPods []*v1.Pod, err error) {

	preemptorKey, err := util.PodKey(preemptor)
	if err != nil {
		return nil, nil, nil, err
	}

	if !podEligibleToPreemptOthers(preemptor, nodeInfoMap) {
		log.L.Debugf("Pod %s is not eligible for more preemption", preemptorKey)
		return nil, nil, nil, nil
	}

	allNodes, err := nodeLister.List()
	if err != nil {
		return nil, nil, nil, err
	}

	if len(allNodes) == 0 {
		return nil, nil, nil, core.ErrNoNodesAvailable
	}

	potentialNodes := nodesWherePreemptionMightHelp(allNodes, fitError.FailedPredicates)
	if len(potentialNodes) == 0 {
		log.L.Debugf("Preemption will not help schedule pod %s on any node.", preemptorKey)
		// In this case, we should clean-up any existing nominated node name of the pod.
		return nil, nil, []*v1.Pod{preemptor}, nil
	}

	// pdbs, err := sched.pdbLister.List(labels.Everything())
	// if err != nil {
	// 	return nil, nil, nil, err
	// }

	nodeToVictims, err := sched.selectNodesForPreemption(preemptor, nodeInfoMap, potentialNodes, podQueue /* , pdb */)
	if err != nil {
		return nil, nil, nil, err
	}

	// // We will only check nodeToVictims with extenders that support preemption.
	// // Extenders which do not support preemption may later prevent preemptor from being scheduled on the nominated
	// // node. In that case, scheduler will find a different host for the preemptor in subsequent scheduling cycles.
	// nodeToVictims, err = g.processPreemptionWithExtenders(pod, nodeToVictims)
	// if err != nil {
	// 	return nil, nil, nil, err
	// }

	candidateNode := pickOneNodeForPreemption(nodeToVictims)
	if candidateNode == nil {
		return nil, nil, nil, nil
	}

	// Lower priority pods nominated to run on this node, may no longer fit on this node.
	// So, we should remove their nomination.
	// Removing their nomination updates these pods and moves them to the active queue.
	// It lets scheduler find another place for them.
	nominatedPods := getLowerPriorityNominatedPods(preemptor, candidateNode.Name, podQueue)
	if nodeInfo, ok := nodeInfoMap[candidateNode.Name]; ok {
		return nodeInfo.Node(), nodeToVictims[candidateNode].Pods, nominatedPods, nil
	}

	return nil, nil, nil, fmt.Errorf("No node named %s in nodeInfoMap", candidateNode.Name)
}

func (sched *ProposedScheduler) selectNodesForPreemption(
	preemptor *v1.Pod,
	nodeInfoMap map[string]*nodeinfo.NodeInfo,
	potentialNodes []*v1.Node,
	podQueue queue.PodQueue,
	// pdbs []*policy.PodDisruptionBudget,
) (map[*v1.Node]*api.Victims, error) {
	nodeToVictims := map[*v1.Node]*api.Victims{}

	for _, node := range potentialNodes {
		pods, numPDBViolations, fits := sched.selectVictimsOnNode(preemptor, nodeInfoMap[node.Name], podQueue /* , pdbs */)
		if fits {
			nodeToVictims[node] = &api.Victims{
				Pods:             pods,
				NumPDBViolations: numPDBViolations,
			}
		}
	}

	return nodeToVictims, nil
}

func (sched *ProposedScheduler) selectVictimsOnNode(
	preemptor *v1.Pod,
	nodeInfo *nodeinfo.NodeInfo,
	podQueue queue.PodQueue,
	// pdbs []*policy.PodDisruptionBudget,
) (pods []*v1.Pod, numPDBViolations int, fits bool) {
	if nodeInfo == nil {
		return nil, 0, false
	}

	potentialVictims := kutil.SortableList{CompFunc: kutil.HigherPriorityPod}
	nodeInfoCopy := nodeInfo.Clone()

	removePod := func(p *v1.Pod) {
		nodeInfoCopy.RemovePod(p)
	}

	addPod := func(p *v1.Pod) {
		nodeInfoCopy.AddPod(p)
	}

	podPriority := util.PodPriority(preemptor)
	for _, p := range nodeInfoCopy.Pods() {
		if util.PodPriority(p) < podPriority {
			potentialVictims.Items = append(potentialVictims.Items, p)
			removePod(p)
		}
	}
	potentialVictims.Sort()

	if fits, _, err := podFitsOnNode(preemptor, sched.predicates, nodeInfoCopy, podQueue); !fits {
		if err != nil {
			log.L.Warnf("Encountered error while selecting victims on node %s: %v", nodeInfoCopy.Node().Name, err)
		}

		log.L.Debugf(
			"Preemptor does not fit in node %s even if all lower-priority pods were removed",
			nodeInfoCopy.Node().Name)
		return nil, 0, false
	}

	var victims []*v1.Pod
	// numViolatingVictim := 0

	// // Try to reprieve as many pods as possible. We first try to reprieve the PDB
	// // violating victims and then other non-violating ones. In both cases, we start
	// // from the highest priority victims.
	// violatingVictims, nonViolatingVictims := filterPodsWithPDBViolation(potentialVictims.Items, pdbs)

	reprievePod := func(p *v1.Pod) bool {
		addPod(p)
		fits, _, _ := podFitsOnNode(preemptor, sched.predicates, nodeInfoCopy, podQueue)
		if !fits {
			removePod(p)
			victims = append(victims, p)

			if l.IsDebugEnabled() {
				key, err := util.PodKey(p)
				if err != nil {
					log.L.Warnf("Encountered error while building key of pod %v: %v", p, err)
					return fits
				}
				log.L.Debugf("Pod %s is a potential preemption victim on node %s.", key, nodeInfoCopy.Node().Name)
			}
		}

		return fits
	}

	for _, p := range /* violatingVictims */ potentialVictims.Items {
		if !reprievePod(p.(*v1.Pod)) {
			// numViolatingVictim++
		}
	}

	// // Now we try to reprieve non-violating victims.
	// for _, p := range nonViolatingVictims {
	// 	reprievePod(p)
	// }

	return victims /* numViolatingVictim */, 0, true
}
