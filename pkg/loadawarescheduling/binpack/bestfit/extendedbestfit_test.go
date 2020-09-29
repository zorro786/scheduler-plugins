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
	"testing"

	"github.com/stretchr/testify/assert"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"

)

func TestCombineScoresV1(t *testing.T) {
	tests := []struct {
		test           string
		ptsNodeScores  framework.NodeScoreList
		bfbpNodeScores framework.NodeScoreList
		expected       framework.NodeScoreList
	} {
		{
			test: "equal PTS scoring",
			ptsNodeScores: framework.NodeScoreList{
				{Name: "node-1", Score: 100},
				{Name: "node-2", Score: 100},
				{Name: "node-3", Score: 100},
			},
			bfbpNodeScores: framework.NodeScoreList{
				{Name: "node-1", Score: 35},
				{Name: "node-2", Score: 55},
				{Name: "node-3", Score: 60},
			},
			expected: framework.NodeScoreList{
				{Name: "node-1", Score: 97},
				{Name: "node-2", Score: 99},
				{Name: "node-3", Score: 100},
			},
		},
		{
			test: "unequal PTS scoring",
			ptsNodeScores: framework.NodeScoreList{
				{Name: "node-1", Score: 100},
				{Name: "node-2", Score: 67},
				{Name: "node-3", Score: 100},
				{Name: "node-4", Score: 82},
				{Name: "node-5", Score: 0},
			},
			bfbpNodeScores: framework.NodeScoreList{
				{Name: "node-1", Score: 35},
				{Name: "node-2", Score: 75},
				{Name: "node-3", Score: 80},
				{Name: "node-4", Score: 90},
				{Name: "node-5", Score: 90},
			},
			expected: framework.NodeScoreList{
				{Name: "node-1", Score: 97},
				{Name: "node-2", Score: 61},
				{Name: "node-3", Score: 100},
				{Name: "node-4", Score: 81},
				{Name: "node-5", Score: 0},

			},
		},
	}
	e := ExtendedBestFit{}
	for _, tt := range tests {
		e.combineScoresV1(tt.ptsNodeScores, tt.bfbpNodeScores)
		assert.ElementsMatch(t, tt.expected, tt.bfbpNodeScores)
	}
}

func TestCombineScoresV2(t *testing.T) {
	tests := []struct {
		test           string
		ptsNodeScores  framework.NodeScoreList
		bfbpNodeScores framework.NodeScoreList
		expected       framework.NodeScoreList
	} {
		{
			test: "equal PTS scoring",
			ptsNodeScores: framework.NodeScoreList{
				{Name: "node-1", Score: 100},
				{Name: "node-2", Score: 100},
				{Name: "node-3", Score: 100},
			},
			bfbpNodeScores: framework.NodeScoreList{
				{Name: "node-1", Score: 35},
				{Name: "node-2", Score: 55},
				{Name: "node-3", Score: 60},
			},
			expected: framework.NodeScoreList{
				{Name: "node-1", Score: 97},
				{Name: "node-2", Score: 99},
				{Name: "node-3", Score: 100},
			},
		},
		{
			test: "unequal PTS scoring",
			ptsNodeScores: framework.NodeScoreList{
				{Name: "node-1", Score: 100},
				{Name: "node-2", Score: 67},
				{Name: "node-3", Score: 100},
				{Name: "node-4", Score: 82},
				{Name: "node-5", Score: 0},
			},
			bfbpNodeScores: framework.NodeScoreList{
				{Name: "node-1", Score: 35},
				{Name: "node-2", Score: 75},
				{Name: "node-3", Score: 80},
				{Name: "node-4", Score: 90},
				{Name: "node-5", Score: 90},
			},
			expected: framework.NodeScoreList{
				{Name: "node-1", Score: 95},
				{Name: "node-2", Score: 66},
				{Name: "node-3", Score: 100},
				{Name: "node-4", Score: 82},
				{Name: "node-5", Score: 0},

			},
		},
	}
	e := ExtendedBestFit{}
	for _, tt := range tests {
		e.combineScoresV2(&tt.ptsNodeScores, &tt.bfbpNodeScores)
		assert.ElementsMatch(t, tt.expected, tt.bfbpNodeScores)
	}
}