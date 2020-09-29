/*
Copyright 2020 The Kubernetes Authors.

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

/**
bestfit package provides K8s scheduler plugins for best-fit variant of bin packing based on CPU utilisation.
It contains plugins for PreScore, Score and PostBind extension points
*/
package bestfit

import (
	"context"
	"encoding/json"
	"fmt"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"math"
	"net/http"
	"sync"
	"time"

	"metrics/loadwatcher/pkg/watcher"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/podtopologyspread"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
)

const (
	hostCPUThresholdPercent     = 40
	staleMetricsIntervalSeconds = 60
	requestMultiplier           = 1.5
	defaultRequestsMilliCores   = 1000
	httpClientTimeoutSeconds    = 55 * time.Second
	preScoreStateKey            = "PreScore" + Name
	cacheCleanupIntervalMinutes = 5

	Name = "BestFitBinPack"
)

var (
	_               framework.PreScorePlugin = &BestFit{}
	_               framework.ScorePlugin    = &BestFit{}
	_               framework.PostBindPlugin = &BestFit{}
	watcherHostName                          = "loadwatcher.svc.local:2020"
	watcherBaseUrl                           = "/watcher"
)

type podInfo struct {
	timestamp int64
	pod       *v1.Pod
}

type BestFit struct {
	mu                   sync.RWMutex
	handle               framework.FrameworkHandle
	extendedBestFit      ExtendedBestFit
	scheduledPodsCache   map[string][]podInfo // Maintains the node-name to podInfo mapping for pods successfully bound in previous cycles
	client               http.Client
}

type preScoreState struct {
	metrics watcher.WatcherMetrics
}

func (s *preScoreState) Clone() framework.StateData {
	return s
}

func (pl *BestFit) PreScore(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodes []*v1.Node) *framework.Status {
	req, _ := http.NewRequest(http.MethodGet, watcherHostName+watcherBaseUrl, nil)
	req.Header.Set("Content-Type", "application/json")

	resp, err := pl.client.Do(req)
	if err != nil {
		klog.Errorf("request to watcher failed: %v", err)
	}
	defer resp.Body.Close()
	klog.V(6).Infof("received status code %v from watcher", resp.StatusCode)
	if resp.StatusCode == http.StatusOK {
		var metrics watcher.WatcherMetrics
		err = json.NewDecoder(resp.Body).Decode(&metrics)
		if err != nil {
			klog.Errorf("unable to decode watcher metrics: %v", err)
			return nil
		}
		state := &preScoreState{
			metrics: metrics,
		}
		cycleState.Write(preScoreStateKey, state)
	}
	return nil
}

func New(obj runtime.Object, handle framework.FrameworkHandle) (framework.Plugin, error) {
	if obj == nil {
		obj = &config.PodTopologySpreadArgs{}
	}
	ptsPl, err := podtopologyspread.New(obj, handle)
	if err != nil {
		return nil, err
	}
	pts, _ := ptsPl.(*podtopologyspread.PodTopologySpread)

	ebfbp := ExtendedBestFit{
		pts:              pts,
		ptsArgs:          pts.BuildArgs().(config.PodTopologySpreadArgs),
		services:         handle.SharedInformerFactory().Core().V1().Services().Lister(),
		replicationCtrls: handle.SharedInformerFactory().Core().V1().ReplicationControllers().Lister(),
		replicaSets:      handle.SharedInformerFactory().Apps().V1().ReplicaSets().Lister(),
		statefulSets:     handle.SharedInformerFactory().Apps().V1().StatefulSets().Lister(),
	}

	pl :=  &BestFit{
		mu:                 sync.RWMutex{},
		handle:             handle,
		extendedBestFit:    ebfbp,
		scheduledPodsCache: make(map[string][]podInfo),
		client: http.Client{
			Timeout: httpClientTimeoutSeconds,
		},
	}

	cacheCleanerTicker := time.NewTicker(time.Minute*cacheCleanupIntervalMinutes)
	go func() {
		for range cacheCleanerTicker.C {
			pl.cleanupCaches()
		}
	} ()

	return pl, nil
}

func (pl *BestFit) Name() string {
	return Name
}

func (pl *BestFit) cleanupCaches() {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	for nodeName, _ := range pl.scheduledPodsCache {
		cache := pl.scheduledPodsCache[nodeName]
		curTime := time.Now().Unix()
		for i := len(cache) - 1; i >= 0; i-- {
			if curTime - cache[i].timestamp > staleMetricsIntervalSeconds {
				n := copy(cache, cache[i+1:])
				for j := n; j < len(cache); j++ {
					cache[j] = podInfo{}
				}
				cache = cache[:n]
				break
			}
		}
		if len(cache) == 0 {
			delete(pl.scheduledPodsCache, nodeName)
		} else {
			pl.scheduledPodsCache[nodeName] = cache
		}
	}
}

func (pl *BestFit) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	status := pl.extendedBestFit.score(ctx, cycleState, pod, nodeName)
	if !status.IsSuccess() {
		klog.Error("ebfbp score failure")
		return 0, status
	}
	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	state, err := getPreScoreState(cycleState)
	if err != nil {
		return 0, framework.NewStatus(framework.Error, err.Error())
	}
	if _, ok := state.metrics.Data.NodeMetricsMap[nodeName]; !ok { // This means the node is new (no metrics yet) or metrics are unavailable
		return framework.MinNodeScore, nil // Avoid the node by scoring minimum
		//TODO(aqadeer): If this happens for a long time, fall back to allocation based bestfit. This could mean maintaining failure state across cycles if scheduler doesn't provide this state
	}

	var curPodCPUUsage int64
	for _, container := range pod.Spec.Containers {
		curPodCPUUsage += PredictedUtilisation(&container)
	}
	if pod.Spec.Overhead != nil {
		curPodCPUUsage += pod.Spec.Overhead.Cpu().MilliValue()
	}

	var nodeCPUUtilPercent float64
	for _, metric := range state.metrics.Data.NodeMetricsMap[nodeName].Metrics {
		if metric.Type == watcher.CPU {
			nodeCPUUtilPercent = metric.Value
		}
	}
	nodeCPUCapMillis := float64(nodeInfo.Node().Status.Capacity.Cpu().MilliValue())
	nodeCPUUtilMillis := (nodeCPUUtilPercent / 100) * nodeCPUCapMillis

	var missingCPUUtilMillis int64 = 0
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	if _, ok := pl.scheduledPodsCache[nodeName]; ok {
		for _, info := range pl.scheduledPodsCache[nodeName] {
			if info.timestamp > state.metrics.Window.End || info.timestamp <= state.metrics.Window.End &&
				(state.metrics.Window.End-info.timestamp) < staleMetricsIntervalSeconds {
				for _, container := range info.pod.Spec.Containers {
					missingCPUUtilMillis += PredictedUtilisation(&container)
				}
				missingCPUUtilMillis += info.pod.Spec.Overhead.Cpu().MilliValue()
			}
		}
		klog.V(6).Infof("missing utilisation for node %v : %v", nodeName, missingCPUUtilMillis)
	}

	var predictedCPUUsage float64
	if nodeCPUCapMillis != 0 {
		predictedCPUUsage = 100 * (nodeCPUUtilMillis + float64(curPodCPUUsage) + float64(missingCPUUtilMillis)) / nodeCPUCapMillis
	}
	if predictedCPUUsage > hostCPUThresholdPercent {
		if predictedCPUUsage > 100 {
			return framework.MinNodeScore, framework.NewStatus(framework.Success, "")
		}
		penalisedScore := 100 - predictedCPUUsage
		klog.V(6).Infof("penalised score for host %v: %v", nodeName, int64(math.Round(penalisedScore)))
		return int64(math.Round(penalisedScore)), framework.NewStatus(framework.Success, "")
	}

	score := predictedCPUUsage + hostCPUThresholdPercent
	klog.V(6).Infof("score for host %v: %v", nodeName, int64(math.Round(score)))
	return int64(math.Round(score)), framework.NewStatus(framework.Success, "")
}

func (pl *BestFit) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

func (pl *BestFit) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	status := pl.extendedBestFit.normalizeScores(ctx, state, pod, &scores)
	if status != nil {
		return status
	}
	klog.V(6).Infof("final normalized scores: %v", scores)
	return nil
}

func (pl *BestFit) PostBind(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.scheduledPodsCache[nodeName] = append(pl.scheduledPodsCache[nodeName], podInfo{timestamp: time.Now().Unix(), pod: p})
}

func PredictedUtilisation(container *v1.Container) int64 {
	if _, ok := container.Resources.Limits[v1.ResourceCPU]; ok {
		return container.Resources.Limits.Cpu().MilliValue()
	} else if _, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
		return int64(math.Round(float64(container.Resources.Requests.Cpu().MilliValue()) * requestMultiplier))
	} else {
		return defaultRequestsMilliCores
	}
}

func getPreScoreState(cycleState *framework.CycleState) (*preScoreState, error) {
	c, err := cycleState.Read(preScoreStateKey)
	if err != nil {
		return nil, fmt.Errorf("error reading %q from cycleState: %v", preScoreStateKey, err)
	}

	s, ok := c.(*preScoreState)
	if !ok {
		return nil, fmt.Errorf("%+v  convert to preScoreState error", c)
	}
	return s, nil
}
