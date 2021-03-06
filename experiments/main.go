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
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/scheduler/algorithm/predicates"
	"k8s.io/kubernetes/pkg/scheduler/algorithm/priorities"

	kubesim "github.com/pfnet-research/k8s-cluster-simulator/pkg"
	"github.com/pfnet-research/k8s-cluster-simulator/pkg/clock"
	"github.com/pfnet-research/k8s-cluster-simulator/pkg/queue"
	"github.com/pfnet-research/k8s-cluster-simulator/pkg/scheduler"
	kutil "k8s.io/kubernetes/pkg/scheduler/util"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.L.WithError(err).Fatal("Error executing root command")
	}
}

const (
	BEST_FIT  = "bestfit"
	WOSRT_FIT = "worstfit"
	OVER_SUB  = "oversub"
	ONE_SHOT  = "oneshot"
	PROPOSED  = "proposed"
	GENERTIC  = "generic"
)

// configPath is the path of the config file, defaulting to "config".
var (
	configPath           string
	isGenWorkload        = false
	isConvertTrace       = false
	isMultipleResource   = false
	isDistributedTasks   = true
	workloadPath         string
	tracePath            string
	targetNum            = 64 * 4
	totalPodsNum         = uint64(10)
	workloadSubsetFactor = int(1)
	submittedPodsNum     = uint64(0)
	predictionPenalty    = float32(1.0)
	targetQoS            = float32(0.0)
	penaltyUpdate        = float32(0.99)
	penaltyTimeout       = int(1)
	podMap               = make(map[string][]string)
	// schedulerName    = "bestfit"
	schedulerName = "default"
	// schedulerName       = "proposed"
	globalOverSubFactor = 4.0

	meanSec              = 2.0
	meanCpu              = 4.0
	cpuStd               = 3.0
	phasNum              = 5
	requestCpu           = 8.0
	requestMem           = 0.001
	startClockStr        = "2019-01-01T00:00:00+09:00"
	endClockStr          = "3019-01-01T00:00:00+09:00"
	startTimestampTrace  = "0"
	tick                 = 1
	nodeMaxCap           = []int{64 * 1000, 128 * 1024, 1 * 1024 * 1024}
	workloadSubfolderCap = 2
	loadPhaseCache       = 10
	queueClass           = 0
	priorityType         = 0
	demandToRequestRatio = float64(1.0)
)

const workerNum = 16

func init() {
	log.L.Infof("Running KubeSim @ %s", time.Now().Format(time.RFC850))
	rootCmd.PersistentFlags().StringVar(
		&configPath, "config", "./config/c2node", "config file (excluding file extension)")
	rootCmd.PersistentFlags().StringVar(
		&workloadPath, "workload", "./config/workload", "config file (excluding file extension)")
	rootCmd.PersistentFlags().BoolVar(
		&isGenWorkload, "isgen", false, "generating workload")
	rootCmd.PersistentFlags().StringVar(
		&schedulerName, "scheduler", "default", "scheduler name")
	rootCmd.PersistentFlags().Float64Var(
		&globalOverSubFactor, "oversub", 1.0, "over sub factor")
	rootCmd.PersistentFlags().BoolVar(
		&isConvertTrace, "istrace", false, "convert trace's csv to json")
	rootCmd.PersistentFlags().BoolVar(
		&isDistributedTasks, "is-distribute", true, "distribute tasks from the same jobs.")
	rootCmd.PersistentFlags().BoolVar(
		&isMultipleResource, "is-multiple-resource", true, "distribute tasks from the same jobs.")
	rootCmd.PersistentFlags().StringVar(
		&tracePath, "trace", "./data/sample/tasks", "config file (excluding file extension)")
	rootCmd.PersistentFlags().StringVar(
		&startClockStr, "start", "2019-01-01T00:00:00+09:00", "start clock")
	rootCmd.PersistentFlags().StringVar(
		&endClockStr, "end", "3019-01-01T00:00:00+09:00", "end clock")
	rootCmd.PersistentFlags().IntVar(
		&tick, "tick", 1, "schedule tick/metrics tick time")
	rootCmd.PersistentFlags().StringVar(
		&startTimestampTrace, "trace-start", "600000000", "start of time stamp in the trace")
	rootCmd.PersistentFlags().Uint64Var(
		&totalPodsNum, "total-pods-num", 1, "totalPodsNum")
	rootCmd.PersistentFlags().IntVar(
		&targetNum, "target-pod-num", 10, "target pod num per time slot")
	rootCmd.PersistentFlags().IntVar(
		&workloadSubsetFactor, "subset-factor", 1, "subset factor of workload trace")
	rootCmd.PersistentFlags().IntVar(
		&workloadSubfolderCap, "workload-subfolder-cap", 10000, "number of pods/jobs per folder")
	rootCmd.PersistentFlags().IntVar(
		&penaltyTimeout, "penalty-timeout", 10, "number of time slots to reset penalty")
	rootCmd.PersistentFlags().Float32Var(
		&predictionPenalty, "prediction-penalty", 1.0, "penalty")
	rootCmd.PersistentFlags().Float32Var(
		&targetQoS, "target-qos", 1.0, "target qos")
	rootCmd.PersistentFlags().Float32Var(
		&penaltyUpdate, "penalty-update", 1.0, "target qos")
	rootCmd.PersistentFlags().IntVar(
		&loadPhaseCache, "load-phase-cache", 100, "number of phases in a pod loaded at a time")
	rootCmd.PersistentFlags().IntVar(
		&queueClass, "queue-class", 0, "queue class: 0: FIFO, 1: PriorityQueue")
	rootCmd.PersistentFlags().IntVar(
		&priorityType, "priority-type", 0, "priority type: default=0, large-to-small-request=1")
	rootCmd.PersistentFlags().Float64Var(
		&demandToRequestRatio, "demand-to-request-ratio", 1.0, "scale up or down demand")
}

var rootCmd = &cobra.Command{
	Use:   "k8s-cluster-simulator",
	Short: "k8s-cluster-simulator provides a virtual kubernetes cluster interface for evaluating your scheduler.",

	Run: func(cmd *cobra.Command, args []string) {
		ctx := newInterruptableContext()

		// 1. Create a KubeSim with a pod queue and a scheduler.
		sched := buildScheduler() // see below
		if sched == nil {
			return
		}

		var q queue.PodQueue
		if queueClass == 0 {
			q = queue.NewFIFOQueue()
		} else if queueClass == 1 {
			q = queue.NewPriorityQueue(priorityType)
		}

		endClock, err := BuildClock(endClockStr, 0)

		kubesim := kubesim.NewKubeSimFromConfigPathOrDie(configPath, q, sched, endClock)
		nodes, _ := kubesim.List()
		for _, node := range nodes {
			predicates.NodesOverSubFactors[node.Name] = globalOverSubFactor
		}
		// 2. Prepare the set of podsubmit time: set<timestamp>

		// 3. Register one or more pod s to KubeSim.

		if err != nil {
			log.L.Fatal(err)
		}
		kubesim.AddSubmitter("MySubmitter", newMySubmitter(totalPodsNum, endClock))

		// 4. Run the main loop of KubeSim.
		//    In each execution of the loop, KubeSim
		//      1) stores pods submitted from the registered submitters to its queue,
		//      2) invokes scheduler with pending pods and cluster state,
		//      3) emits cluster metrics to designated location(s) if enabled
		//      4) progresses the simulated clock
		if err := kubesim.Run(ctx); err != nil && errors.Cause(err) != context.Canceled {
			log.L.Fatal(err)
		}
	},
}

func convertTrace2Workload(tracePath string, workloadPath string) {
	startTimestamp, _ := strconv.Atoi(startTimestampTrace)
	startTimestamp = startTimestamp / MICRO_SECONDS
	endClock, err := BuildClock(endClockStr, 0)
	if err != nil {
		log.L.Errorf("endClockStr format is not correct, %v", endClockStr)
	}
	startClock, err := BuildClock(startClockStr, 0)
	if err != nil {
		log.L.Errorf("startClockStr format is not correct, %v", startClockStr)
	}
	if endClock.Before(startClock) {
		log.L.Errorf("startClockStr/endClockStr format is not correct, %v, %v", startClockStr, endClockStr)
	}

	var paths []string
	err = filepath.Walk(tracePath,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if strings.Contains(path, "csv") {
				timestamp := ArrivalTimeInSeconds(path)
				arrivalClock, _ := BuildClock(startClockStr, int64(timestamp-startTimestamp))
				if !endClock.Before(arrivalClock) && !arrivalClock.Before(startClock) {
					paths = append(paths, path)
				}
			}
			return nil
		})
	if err != nil {
		log.L.Println(err)
	}

	fileNum := len(paths)
	sortableList := kutil.SortableList{CompFunc: TaskInChronologicalOrder}
	for i := 0; i < fileNum; i++ {
		sortableList.Items = append(sortableList.Items, paths[i])
	}
	sortableList.Sort()
	log.L.Infof("Total number of selected files to generate workload: %v", len(paths))

	// Generate json files in parrallel

	if fileNum > int(totalPodsNum) {
		fileNum = int(totalPodsNum)
	}
	parralel := true

	if !isMultipleResource {
		nodeMaxCap[1] = 0
		nodeMaxCap[2] = 0
	}

	if parralel {
		ctx, _ := context.WithCancel(context.Background())
		workqueue.ParallelizeUntil(ctx, workerNum, int(fileNum), func(i int) {
			if i*workloadSubsetFactor >= fileNum {
				return
			}
			filePath := string(sortableList.Items[i*workloadSubsetFactor].(string))
			timestamp := int(ArrivalTimeInSeconds(filePath)/tick) * tick // rounding
			startClock, _ := BuildClock(startClockStr, int64(timestamp-startTimestamp))
			maxLength := int(endClock.Sub(startClock).Seconds())
			pod, err := ConvertTraceToPod(filePath, "0", nodeMaxCap[0], nodeMaxCap[1], maxLength, demandToRequestRatio)
			if err == nil && maxLength > 0 {
				subWorkload := strconv.Itoa(i / workloadSubfolderCap)
				WritePodAsJson(*pod, workloadPath+"/"+subWorkload, startClock)
			}
		})
	} else {
		for i := 0; i < fileNum*workloadSubsetFactor; i += workloadSubsetFactor {
			if i*workloadSubsetFactor >= fileNum {
				break
			}
			filePath := string(sortableList.Items[i*workloadSubsetFactor].(string))
			timestamp := int(ArrivalTimeInSeconds(filePath)/tick) * tick // rounding
			startClock, _ := BuildClock(startClockStr, int64(timestamp-startTimestamp))
			maxLength := int(endClock.Sub(startClock).Seconds())
			pod, err := ConvertTraceToPod(filePath, "0", nodeMaxCap[0], nodeMaxCap[1], maxLength, demandToRequestRatio)
			if err == nil && maxLength > 0 {
				subWorkload := strconv.Itoa(i / workloadSubfolderCap)
				WritePodAsJson(*pod, workloadPath+"/"+subWorkload, startClock)
			}
		}
	}

}

func buildScheduler() scheduler.Scheduler {
	if isGenWorkload {
		start := time.Now()
		log.L.Infof("Generating %v pods", totalPodsNum)
		os.RemoveAll(workloadPath)
		os.MkdirAll(workloadPath, 0755)
		if isConvertTrace {
			convertTrace2Workload(tracePath, workloadPath)
			return nil
		}
		lapse := time.Since(start)
		if _, ok := scheduler.TimingMap["convertTrace2Workload"]; !ok {
			scheduler.TimingMap["convertTrace2Workload"] = lapse.Microseconds()
		} else {
			scheduler.TimingMap["convertTrace2Workload"] += lapse.Microseconds()
		}
	}

	start := time.Now()
	count := uint64(0)
	err := filepath.Walk(workloadPath,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if strings.Contains(path, "json") {
				arr := strings.Split(path, "/")
				l := len(arr)
				fileName := string(arr[l-1])
				arr = strings.Split(fileName, "@")
				clockStr := arr[0]
				if _, ok := podMap[clockStr]; ok {
					podMap[clockStr] = append(podMap[clockStr], path)
				} else {
					strArr := []string{path}
					podMap[clockStr] = strArr
				}
				count++
			}
			return nil
		})
	if err != nil {
		log.L.Println(err)
	}
	if !isGenWorkload {
		totalPodsNum = count
		log.L.Infof("Total number of pods in the workload folder: %v", totalPodsNum)
	}

	lapse := time.Since(start)
	if _, ok := scheduler.TimingMap["loadWorkload"]; !ok {
		scheduler.TimingMap["loadWorkload"] = lapse.Microseconds()
	} else {
		scheduler.TimingMap["loadWorkload"] += lapse.Microseconds()
	}

	log.L.Infof("scheduler input %s", schedulerName)
	log.L.Infof("Submitting %d pods", totalPodsNum)
	log.L.Infof("workload: %s", workloadPath)
	log.L.Infof("isGenWorkload: %v", isGenWorkload)
	log.L.Infof("isConvertTrace: %v", isConvertTrace)
	log.L.Infof("trace: %v", tracePath)
	log.L.Infof("cluster: %s", configPath)
	log.L.Infof("oversub: %f", globalOverSubFactor)
	log.L.Infof("workloadSubsetFactor: %v", workloadSubsetFactor)
	log.L.Infof("tick: %d", tick)
	log.L.Infof("start: %v", startClockStr)
	log.L.Infof("endClockStr: %v", endClockStr)
	log.L.Infof("predictionPenalty: %v", predictionPenalty)
	log.L.Infof("targetQoS: %v", targetQoS)
	log.L.Infof("penaltyTimeout: %v", penaltyTimeout)
	log.L.Infof("penaltyUpdate: %v", penaltyUpdate)
	log.L.Infof("loadPhaseCache: %v", loadPhaseCache)
	log.L.Infof("queueClass: %v", queueClass)
	log.L.Infof("priorityType: %v", priorityType)
	log.L.Infof("isDistributedTasks: %v", isDistributedTasks)
	log.L.Infof("isMultipleResource: %v", isMultipleResource)
	log.L.Infof("demandToRequestRatio: %v", demandToRequestRatio)

	scheduler.PredictionPenalty = predictionPenalty
	scheduler.PenaltyTimeout = penaltyTimeout
	scheduler.TargetQoS = targetQoS
	scheduler.PenaltyUpdate = penaltyUpdate

	clock.LOAD_PHASE_CACHE = loadPhaseCache

	switch schedName := strings.ToLower(schedulerName); schedName {
	// case ONE_SHOT:
	// 	log.L.Infof("Scheduler: %s", ONE_SHOT)
	// 	globalOverSubFactor = 1.0
	// 	sched := scheduler.NewOneShotScheduler(false, predictionPenalty, penaltyTimeout)
	// 	// 2. Register extender(s)
	// 	sched.AddExtender(
	// 		scheduler.Extender{
	// 			Name:             "MyExtender",
	// 			Filter:           filterExtender,
	// 			Prioritize:       prioritizeExtender,
	// 			Weight:           1,
	// 			NodeCacheCapable: true,
	// 		},
	// 	)

	// 	// 2. Register plugin(s)
	// 	// Predicate
	// 	sched.AddPredicate("JobConfictPredicates", predicates.JobConfict)
	// 	// Prioritizer
	// 	sched.AddPrioritizer(priorities.PriorityConfig{
	// 		Name:   "MostRequested",
	// 		Map:    priorities.MostRequestedPriorityMap,
	// 		Reduce: nil,
	// 		Weight: 1,
	// 	})

	// 	return &sched
	case PROPOSED:
		log.L.Infof("Scheduler: %s", PROPOSED)
		globalOverSubFactor = 1.0
		sched := scheduler.NewGenericScheduler(false)
		// 2. Register extender(s)
		sched.AddExtender(
			scheduler.Extender{
				Name:             "filterFitResource & prioritizeLowUsageNode",
				Filter:           filterFitResource,
				Prioritize:       prioritizeLowUsageNode,
				Weight:           1,
				NodeCacheCapable: true,
			},
		)

		// 2. Register plugin(s)
		// Predicate
		if isDistributedTasks {
			// Prioritizer
			sched.AddPrioritizer(priorities.PriorityConfig{
				Name:   "AvoidTasksFromSameJob",
				Map:    priorities.LeastTasksFromSameJobPriorityMap,
				Reduce: nil,
				Weight: 1,
			})
		}

		return &sched
	case OVER_SUB:
		log.L.Infof("Scheduler: %s", OVER_SUB)
		sched := scheduler.NewGenericScheduler(false)

		// 2. Register plugin(s)
		// Predicate
		sched.AddPredicate("PodFitsResourcesOverSub", predicates.PodFitsResourcesOverSub)
		// if isDistributedTasks {
		// 	sched.AddPredicate("JobConfictPredicates", predicates.JobConfict)
		// }
		// Prioritizer
		sched.AddPrioritizer(priorities.PriorityConfig{
			Name: "LeastRequested",
			// Map:    priorities.MostRequestedPriorityMap,
			Map:    priorities.LeastRequestedPriorityMap,
			Reduce: nil,
			Weight: 1,
		})

		return &sched
	case BEST_FIT:
		log.L.Infof("Scheduler: %s", BEST_FIT)
		globalOverSubFactor = 1.0
		sched := scheduler.NewGenericScheduler(false)
		// 2. Register extender(s)
		sched.AddExtender(
			scheduler.Extender{
				Name:             "MyExtender",
				Filter:           filterExtender,
				Prioritize:       prioritizeExtender,
				Weight:           1,
				NodeCacheCapable: true,
			},
		)

		// 2. Register plugin(s)
		// Predicate
		sched.AddPredicate("PodFitsResources", predicates.PodFitsResources)
		// if isDistributedTasks {
		// 	sched.AddPredicate("JobConfictPredicates", predicates.JobConfict)
		// }
		// Prioritizer
		sched.AddPrioritizer(priorities.PriorityConfig{
			Name:   "MostRequested",
			Map:    priorities.MostRequestedPriorityMap,
			Reduce: nil,
			Weight: 1,
		})

		return &sched
	case WOSRT_FIT:
		log.L.Infof("Scheduler: %s", WOSRT_FIT)
		globalOverSubFactor = 1.0
		sched := scheduler.NewGenericScheduler(false)
		// 2. Register plugin(s)
		// Predicate
		sched.AddPredicate("PodFitsResources", predicates.PodFitsResources)
		// if isDistributedTasks {
		// 	sched.AddPredicate("JobConfictPredicates", predicates.JobConfict)
		// }
		// Prioritizer
		sched.AddPrioritizer(priorities.PriorityConfig{
			Name:   "LeastRequested",
			Map:    priorities.LeastRequestedPriorityMap,
			Reduce: nil,
			Weight: 1,
		})

		return &sched
	default:
		log.L.Infof("Scheduler: DEFAULT")
		// 1. Create a generic scheduler that mimics a kube-scheduler.
		sched := scheduler.NewGenericScheduler( /* preemption disabled */ false)
		// 2. Register extender(s)
		sched.AddExtender(
			scheduler.Extender{
				Name:             "MyExtender",
				Filter:           filterExtender,
				Prioritize:       prioritizeExtender,
				Weight:           1,
				NodeCacheCapable: true,
			},
		)

		// 2. Register plugin(s)
		// Predicate
		sched.AddPredicate("GeneralPredicates", predicates.GeneralPredicates)
		// if isDistributedTasks {
		// 	sched.AddPredicate("JobConfictPredicates", predicates.JobConfict)
		// }
		// Prioritizer
		sched.AddPrioritizer(priorities.PriorityConfig{
			Name:   "BalancedResourceAllocation",
			Map:    priorities.BalancedResourceAllocationMap,
			Reduce: nil,
			Weight: 1,
		})
		sched.AddPrioritizer(priorities.PriorityConfig{
			Name:   "LeastRequested",
			Map:    priorities.LeastRequestedPriorityMap,
			Reduce: nil,
			Weight: 1,
		})

		return &sched
	}
}

func newInterruptableContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())

	// SIGINT (Ctrl-C) and SIGTERM cancel kubesim.Run().
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	return ctx
}

// for test
func lifo(pod0, pod1 *v1.Pod) bool { // nolint
	return pod1.CreationTimestamp.Before(&pod0.CreationTimestamp)
}
