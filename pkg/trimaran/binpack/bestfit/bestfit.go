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
	"fmt"
	"github.com/francoispqt/gojay"
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
	pluginConfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
)

const (
	hostCPUThresholdPercent              = 40   // Upper limit of CPU percent for bin packing. Recommended to keep -10 than desired limit.
	metricsAgentReportingIntervalSeconds = 60   // Time interval in seconds for each metrics agent ingestion.
	requestMultiplier                    = 1.5  // CPU usage is predicted as 1.5*requests for containers without limits i.e. Burstable QOS.
	defaultRequestsMilliCores            = 1000 // Default 1 core CPU usage for containers without requests/limits i.e. Best Effort QOS.
	httpClientTimeoutSeconds             = 55 * time.Second
	preScoreStateKey                     = "PreScore-" + Name
	cacheCleanupIntervalMinutes          = 5 // This is the maximum staleness of metrics possible by load watcher
	metricsUpdateIntervalSeconds         = 30

	Name = "BestFitBinPack"
)

var (
	//_               framework.PreScorePlugin = &BestFit{}
	_               framework.ScorePlugin    = &BestFit{}
	_               framework.PostBindPlugin = &BestFit{}
	// Exported for testing
	WatcherHostName                          = "loadwatcher.svc.local:2020"
	WatcherBaseUrl                           = "/watcher"
	W 								watcher.Watcher
)

// Stores timestamp when pod is called in Post Bind phase after binding successfully
type podInfo struct {
	timestamp int64
	pod       *v1.Pod
}

type BestFit struct {
	mu                 sync.RWMutex // RW mutex to guard read access to scheduledPodsCache from Score() and write access from cleanupCache() & PostBind().
	handle             framework.FrameworkHandle
	extendedBestFit    ExtendedBestFit
	scheduledPodsCache map[string][]podInfo // Maintains the node-name to podInfo mapping for pods successfully bound in previous cycles.
	client             http.Client
	metrics            watcher.WatcherMetrics
}

// preScoreState computed at PreScore and used at Score in the same scheduling cycle.
type preScoreState struct {
	metrics watcher.WatcherMetrics
}

func (s *preScoreState) Clone() framework.StateData {
	return s
}

func (pl *BestFit) updateMetrics() error {
	req, err := http.NewRequest(http.MethodGet, WatcherHostName+WatcherBaseUrl, nil)
	if err != nil {
		klog.Errorf("new watcher request failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := pl.client.Do(req) //TODO(aqadeer): Add a couple of retries for transient errors
	if err != nil {
		klog.Errorf("request to watcher failed: %v", err)
		return err
	}
	defer resp.Body.Close()
	klog.V(6).Infof("received status code %v from watcher", resp.StatusCode)
	if resp.StatusCode == http.StatusOK {
		data := watcher.Data{NodeMetricsMap: make(map[string]watcher.NodeMetrics)}
		var metrics = watcher.WatcherMetrics{Data: data}
		dec := gojay.BorrowDecoder(resp.Body)
		defer dec.Release()
		err = dec.Decode(&metrics)
		if err != nil {
			klog.Errorf("unable to decode watcher metrics: %v", err)
		}
		pl.mu.Lock()
		defer pl.mu.Unlock()
		pl.metrics = metrics
	} else {
		klog.Errorf("received status code %v from watcher", resp.StatusCode)
	}
	return nil
}

func New(obj runtime.Object, handle framework.FrameworkHandle) (framework.Plugin, error) {
	ptsArgs, err := getArgs(obj)
	if err != nil {
		return nil, err
	}
	ptsPl, err := podtopologyspread.New(ptsArgs, handle)
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

	go func() {
		cacheCleanerTicker := time.NewTicker(time.Minute*cacheCleanupIntervalMinutes)
		for range cacheCleanerTicker.C {
			pl.cleanupCache()
		}
	} ()

	go func() {
		metricsUpdaterTicker := time.NewTicker(time.Second*metricsUpdateIntervalSeconds)
		for range metricsUpdaterTicker.C {
			pl.updateMetrics()
		}
	} ()

	return pl, nil
}

func (pl *BestFit) Name() string {
	return Name
}

func getArgs(obj runtime.Object) (ptsArgs *config.PodTopologySpreadArgs, err error) {
	if obj == nil {
		ptsArgs = &config.PodTopologySpreadArgs{}
	} else {
		if bestFitArgs, ok := obj.(*pluginConfig.BestFitBinPackArgs); !ok {
			return nil, fmt.Errorf("want args to be of type BestFitBinPackArgs, got %T", obj)
		} else {
			ptsArgs = &bestFitArgs.PTSArgs
		}
	}
	return ptsArgs, nil
}

func (pl *BestFit) cleanupCache() {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	for nodeName, _ := range pl.scheduledPodsCache {
		cache := pl.scheduledPodsCache[nodeName]
		curTime := time.Now().Unix()
		for i := len(cache) - 1; i >= 0; i-- {
			if curTime - cache[i].timestamp > metricsAgentReportingIntervalSeconds {
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

	//state, err := getPreScoreState(cycleState)
	//if err != nil {
	//	return 0, framework.NewStatus(framework.Error, err.Error())
	//}
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	if _, ok := pl.metrics.Data.NodeMetricsMap[nodeName]; !ok { // This means the node is new (no metrics yet) or metrics are unavailable due to 404 or 500
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
	var cpuMetricFound bool
	for _, metric := range pl.metrics.Data.NodeMetricsMap[nodeName].Metrics {
		if metric.Type == watcher.CPU {
			nodeCPUUtilPercent = metric.Value
			cpuMetricFound = true
		}
	}

	if !cpuMetricFound {
		klog.Errorf("cpu metric not found for node %v in node metrics %v", nodeName, pl.metrics.Data.NodeMetricsMap[nodeName].Metrics)
		return 0, nil
	}
	nodeCPUCapMillis := float64(nodeInfo.Node().Status.Capacity.Cpu().MilliValue())
	nodeCPUUtilMillis := (nodeCPUUtilPercent / 100) * nodeCPUCapMillis

	var missingCPUUtilMillis int64 = 0

	if _, ok := pl.scheduledPodsCache[nodeName]; ok {
		for _, info := range pl.scheduledPodsCache[nodeName] {
			// If the time stamp of the scheduled pod is outside fetched metrics window, or it is within metrics reporting interval seconds, we predict util.
			// Note that the second condition doesn't guarantee metrics for that pod are not reported yet as the 0 <= t <= 2*metricsAgentReportingIntervalSeconds
			// t = metricsAgentReportingIntervalSeconds is taken as average case and it doesn't hurt us much if we are
			// counting metrics twice in case actual t is less than metricsAgentReportingIntervalSeconds
			if info.timestamp > pl.metrics.Window.End || info.timestamp <= pl.metrics.Window.End &&
				(pl.metrics.Window.End-info.timestamp) < metricsAgentReportingIntervalSeconds {
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
		penalisedScore := int64(math.Round(100 - predictedCPUUsage))
		klog.V(6).Infof("penalised score for host %v: %v", nodeName, penalisedScore)
		return penalisedScore, framework.NewStatus(framework.Success, "")
	}

	score := int64(math.Round(predictedCPUUsage + hostCPUThresholdPercent))
	klog.V(6).Infof("score for host %v: %v", nodeName, score)
	return int64(score), framework.NewStatus(framework.Success, "")
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

// Predict utilisation for a container based on its requests/limits
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
