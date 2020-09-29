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

package bestfit

import (
	"context"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/helper"
	"math"

	v1 "k8s.io/api/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/podtopologyspread"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
)

const (
	defaultSecondMax = 96
	thirdPrefHi      = 80
	fourthPrefHi     = 60
	fifthPrefHi      = 30
)

type ExtendedBestFit struct {
	pts              *podtopologyspread.PodTopologySpread
	ptsArgs          config.PodTopologySpreadArgs
	ptsScores        framework.NodeScoreList
	services         corelisters.ServiceLister
	replicationCtrls corelisters.ReplicationControllerLister
	replicaSets      appslisters.ReplicaSetLister
	statefulSets     appslisters.StatefulSetLister
}

type preference struct {
	lo            int64
	hi            int64
	nodeScoreList framework.NodeScoreList
}

type buckets struct {
	preferences [5]preference
	size        int
}

func (e *ExtendedBestFit) score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	if !e.checkIfPodNeedsSpreading(pod) {
		return nil
	}
	score, status := e.pts.Score(ctx, state, pod, nodeName)
	if !status.IsSuccess() {
		klog.Errorf("unable to score using PodTopologySpread plugin for pod %v", pod.Name)
		return status
	}
	klog.Infof("pts score for node %v: %v", nodeName, score)
	e.ptsScores = append(e.ptsScores, framework.NodeScore{
		Name:  nodeName,
		Score: score,
	})
	return nil
}

func max(a, b int64) int64 {
	if a >= b {
		return a
	}
	return b
}

// Returns second maximum distinct score value
func (e *ExtendedBestFit) findSecondMax(list framework.NodeScoreList) int64 {
	var max = framework.MinNodeScore
	var secondMax = framework.MinNodeScore
	for _, v := range list {
		if v.Score > max {
			secondMax = max
			max = v.Score
		} else if v.Score > secondMax && v.Score != max {
			secondMax = v.Score
		}
	}
	return secondMax
}

func (e *ExtendedBestFit) combineScoresV1(ptsNodeScores, bfbpNodeScores framework.NodeScoreList) {
	bfbpNodeScoreMap := make(map[string]framework.NodeScore)
	for _, v := range bfbpNodeScores {
		bfbpNodeScoreMap[v.Name] = v
	}

	var secondMax int64
	if len(ptsNodeScores) > 1 {
		secondMax = max(e.findSecondMax(ptsNodeScores), defaultSecondMax)
	}

	// Initialize buckets
	buckets := buckets{
		preferences: [5]preference{
			{hi: framework.MaxNodeScore, lo: secondMax + 1},
			{hi: secondMax, lo: thirdPrefHi + 1},
			{hi: thirdPrefHi, lo: fourthPrefHi + 1},
			{hi: fourthPrefHi, lo: fifthPrefHi + 1},
			{hi: fifthPrefHi, lo: framework.MinNodeScore},
		},
	}

	// Fill preference buckets
	for _, ptsNodeScore := range ptsNodeScores {
		for idx, pref := range buckets.preferences {
			if ptsNodeScore.Score <= pref.hi && ptsNodeScore.Score >= pref.lo {
				buckets.preferences[idx].nodeScoreList = append(buckets.preferences[idx].nodeScoreList, bfbpNodeScoreMap[ptsNodeScore.Name])
				break
			}
		}
	}

	// Normalize BFBP values in each preference bucket and update the values
	for _, pref := range buckets.preferences {
		min, max := getMinMax(pref.nodeScoreList)
		for _, v := range pref.nodeScoreList {
			combinedScore := bfbpNodeScoreMap[v.Name]
			if max-min == 0 {
				combinedScore.Score = pref.lo
			} else {
				combinedScore.Score = int64(float64(pref.hi-pref.lo)*float64(v.Score-min)/float64(max-min)) + pref.lo
			}
			bfbpNodeScoreMap[v.Name] = combinedScore
		}
	}

	// Update scores in original list
	for i, v := range bfbpNodeScores {
		bfbpNodeScores[i].Score = bfbpNodeScoreMap[v.Name].Score
	}
}

func (e *ExtendedBestFit) combineScoresV2(ptsNodeScores, bfbpNodeScores *framework.NodeScoreList) {
	ptsNodeScoreMap := make(map[string]framework.NodeScore)
	var combinedNodeScores framework.NodeScoreList
	for _, ptsNodeScore := range *ptsNodeScores {
		ptsNodeScoreMap[ptsNodeScore.Name] = ptsNodeScore
	}
	var maxCombinedScore int64
	var minCombinedScore = framework.MaxNodeScore
	for _, bfbpNodeScore := range *bfbpNodeScores {
		combinedNodeScore := framework.NodeScore{Name: bfbpNodeScore.Name,
			Score: 1000*ptsNodeScoreMap[bfbpNodeScore.Name].Score + bfbpNodeScore.Score*ptsNodeScoreMap[bfbpNodeScore.Name].Score}

		combinedNodeScores = append(combinedNodeScores, combinedNodeScore)
		if combinedNodeScore.Score > maxCombinedScore {
			maxCombinedScore = combinedNodeScore.Score
		}
		if combinedNodeScore.Score < minCombinedScore {
			minCombinedScore = combinedNodeScore.Score
		}
	}
	for i, combinedNodeScore := range combinedNodeScores {
		combinedNodeScores[i].Score = 100 * (combinedNodeScore.Score - minCombinedScore) / (maxCombinedScore - minCombinedScore)
	}
	copy(*bfbpNodeScores, combinedNodeScores)
}

func (e *ExtendedBestFit) normalizeScores(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores *framework.NodeScoreList) *framework.Status {
	if !e.checkIfPodNeedsSpreading(pod) {
		return nil
	}
	klog.V(10).Infof("pts scores before normalizing: %v", e.ptsScores)
	status := e.pts.NormalizeScore(ctx, state, pod, e.ptsScores)
	if !status.IsSuccess() {
		klog.Errorf("unable to normalize score using PodTopologySpread plugin for pod %v", pod)
		return status
	}
	klog.V(10).Infof("pts scores after normalizing: %v", e.ptsScores)
	e.combineScoresV2(&e.ptsScores, scores)
	e.ptsScores = nil
	return nil
}

func (e *ExtendedBestFit) checkIfPodNeedsSpreading(pod *v1.Pod) bool {
	if len(pod.Spec.TopologySpreadConstraints) == 0 && len(e.ptsArgs.DefaultConstraints) == 0 {
		klog.V(6).Info("no default or pod level constraints found")
		return false
	}
	selector := helper.DefaultSelector(pod, e.services, e.replicationCtrls, e.replicaSets, e.statefulSets)
	if selector.Empty() {
		return false
	}
	return true
}

func getMinMax(nodeScoreList framework.NodeScoreList) (int64, int64) {
	var min int64 = math.MaxInt64
	var max int64 = math.MinInt64
	for _, v := range nodeScoreList {
		if v.Score < min {
			min = v.Score
		}
		if v.Score > max {
			max = v.Score
		}
	}
	return min, max
}
