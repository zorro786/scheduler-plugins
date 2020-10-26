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
	"encoding/json"
	"fmt"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"metrics/loadwatcher/pkg/watcher"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apiRuntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	testClientSet "k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/podtopologyspread"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	pluginConfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
)

var _ framework.SharedLister = &testSharedLister{}

type testSharedLister struct {
	nodes       []*v1.Node
	nodeInfos   []*framework.NodeInfo
	nodeInfoMap map[string]*framework.NodeInfo
}

func (f *testSharedLister) NodeInfos() framework.NodeInfoLister {
	return f
}

func (f *testSharedLister) List() ([]*framework.NodeInfo, error) {
	return f.nodeInfos, nil
}

func (f *testSharedLister) HavePodsWithAffinityList() ([]*framework.NodeInfo, error) {
	return nil, nil
}

func (f *testSharedLister) Get(nodeName string) (*framework.NodeInfo, error) {
	return f.nodeInfoMap[nodeName], nil
}

func TestNew(t *testing.T) {
	registeredPlugins := []st.RegisterPluginFunc{
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		st.RegisterScorePlugin(Name, New, 1),
	}

	cs := testClientSet.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(cs, 0)
	snapshot := newTestSharedLister(nil, nil)
	fh, err := st.NewFramework(registeredPlugins, runtime.WithClientSet(cs),
		runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
	assert.Nil(t, err)
	p, err := New(nil, fh)
	assert.NotNil(t, p)
	assert.Nil(t, err)
}

func TestBestFitScoring(t *testing.T) {

	registeredPlugins := []st.RegisterPluginFunc{
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		st.RegisterScorePlugin(Name, New, 1),
	}

	nodeResources := map[v1.ResourceName]string{
		v1.ResourceCPU:    "1000m",
		v1.ResourceMemory: "1Gi",
	}

	tests := []struct {
		test            string
		pod             *v1.Pod
		nodes           []*v1.Node
		watcherResponse watcher.WatcherMetrics
		expected        framework.NodeScoreList
	}{
		{
			test: "new node",
			pod:  st.MakePod().Name("p").Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data{
					NodeMetricsMap: map[string]watcher.NodeMetrics{
						"node-1": {
							Metrics: []watcher.Metrics{
								{
									Type:  watcher.CPU,
									Value: 0,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: int64(hostCPUThresholdPercent)},
			},
		},
		{
			test: "hot node",
			pod:  st.MakePod().Name("p").Obj(),
			nodes: []*v1.Node{

				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data {
					NodeMetricsMap:  map[string]watcher.NodeMetrics{
						"node-1": {
							Metrics: []watcher.Metrics{
								{
									Type:  watcher.CPU,
									Value: hostCPUThresholdPercent + 10,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: 100 - int64(hostCPUThresholdPercent) - 10},
			},
		},
		{
			test: "excess utilisation returns min score",
			pod:  getPodWithContainersAndOverhead(0, 1000),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data {
					NodeMetricsMap: map[string]watcher.NodeMetrics{
						"node-1": {
							Metrics: []watcher.Metrics{
								{
									Type:  watcher.CPU,
									Value: 30,
								},
							},
						},
					},
				},
			},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: framework.MinNodeScore},
			},
		},
		{
			test: "404 resp from watcher",
			pod:  st.MakePod().Name("p").Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{},
			expected: []framework.NodeScore{
				{Name: "node-1", Score: framework.MinNodeScore},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.test, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
				bytes, err := json.Marshal(tt.watcherResponse)
				assert.Nil(t, err)
				resp.Write(bytes)
			}))
			// point watcher to test server
			WatcherHostName = server.URL
			WatcherBaseUrl = ""

			defer server.Close()

			nodes := append([]*v1.Node{}, tt.nodes...)
			state := framework.NewCycleState()

			cs := testClientSet.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			snapshot := newTestSharedLister(nil, nodes)
			fh, err := st.NewFramework(registeredPlugins, runtime.WithClientSet(cs),
				runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
			assert.Nil(t, err)
			p, err := New(nil, fh)
			scorePlugin := p.(framework.ScorePlugin)
			var actualList framework.NodeScoreList
			for _, n := range tt.nodes {
				nodeName := n.Name
				score, status := scorePlugin.Score(context.Background(), state, tt.pod, nodeName)
				assert.True(t, status.IsSuccess())
				actualList = append(actualList, framework.NodeScore{Name: nodeName, Score: score})
			}
			assert.ElementsMatch(t, tt.expected, actualList)
		})
	}
}

func TestExtendedBestFitScoring(t *testing.T) {
	registeredPlugins := []st.RegisterPluginFunc{
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		st.RegisterPreFilterPlugin(podtopologyspread.Name, podtopologyspread.New),
		st.RegisterFilterPlugin(podtopologyspread.Name, podtopologyspread.New),
		st.RegisterPreScorePlugin(podtopologyspread.Name, podtopologyspread.New),
		st.RegisterScorePlugin(Name, New, 1),
	}

	nodeResources := map[v1.ResourceName]string{
		v1.ResourceCPU:    "1000m",
		v1.ResourceMemory: "1Gi",
	}

	tests := []struct {
		test            string
		pod             *v1.Pod
		existingPods    []*v1.Pod
		nodes           []*v1.Node
		watcherResponse watcher.WatcherMetrics
		expectedScores  framework.NodeScoreList
		expectedMapping string
	}{
		{
			test: "non replica doesn't invoke ebfbp scoring",
			pod:  st.MakePod().Name("p").Obj(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data {
					NodeMetricsMap: map[string]watcher.NodeMetrics{
						"node-1": {
							Metrics: []watcher.Metrics{
								{
									Type:  watcher.CPU,
									Value: 0,
								},
							},
						},
					},
				},
			},
			expectedScores: framework.NodeScoreList{
				{Name: "node-1", Score: int64(hostCPUThresholdPercent)},
			},
		},
		{
			test: "replica pod are spread",
			pod:  st.MakePod().Name("p").Label("pool", "test-app").Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p").Node("node-1").Label("pool", "test-app").Obj(),
				st.MakePod().Name("p").Node("node-2").Label("pool", "test-app").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-1").Label(v1.LabelHostname, "node-1").Capacity(nodeResources).Obj(),
				st.MakeNode().Name("node-2").Label(v1.LabelHostname, "node-2").Capacity(nodeResources).Obj(),
				st.MakeNode().Name("node-3").Label(v1.LabelHostname, "node-3").Capacity(nodeResources).Obj(),
			},
			watcherResponse: watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data{
					NodeMetricsMap: map[string]watcher.NodeMetrics{
						"node-1": {
							Metrics: []watcher.Metrics{
								{
									Type:  watcher.CPU,
									Value: 40,
								},
							},
						},
						"node-2": {
							Metrics: []watcher.Metrics{
								{
									Type:  watcher.CPU,
									Value: 30,
								},
							},
						},
						"node-3": {
							Metrics: []watcher.Metrics{
								{
									Type:  watcher.CPU,
									Value: 10,
								},
							},
						},
					},
				},
			},
			expectedMapping: "node-3",
		},
	}

	obj := []apiRuntime.Object{
		&appsv1.ReplicaSet{Spec: appsv1.ReplicaSetSpec{Selector: st.MakeLabelSelector().Exists("pool").Obj()}},
	}

	bfbpArgs := pluginConfig.BestFitBinPackArgs{}

	for _, tt := range tests {
		t.Run(tt.test, func(t *testing.T) {
			nodes := append([]*v1.Node{}, tt.nodes...)
			state := framework.NewCycleState()

			cs := testClientSet.NewSimpleClientset(obj...)
			informerFactory := informers.NewSharedInformerFactory(cs, 0)

			snapshot := newTestSharedLister(tt.existingPods, nodes)
			fh, err := st.NewFramework(registeredPlugins, runtime.WithClientSet(cs),
				runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
			assert.Nil(t, err)
			pl, err := New(&bfbpArgs, fh)
			assert.Nil(t, err)

			informerFactory.Start(context.Background().Done())
			informerFactory.WaitForCacheSync(context.Background().Done())

			server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
				bytes, err := json.Marshal(tt.watcherResponse)
				assert.Nil(t, err)
				resp.Write(bytes)
			}))
			// point watcher to test server
			WatcherHostName = server.URL
			WatcherBaseUrl = ""

			defer server.Close()

			scorePlugin := pl.(framework.ScorePlugin)
			var actualList framework.NodeScoreList
			for _, n := range tt.nodes {
				nodeName := n.Name
				score, status := scorePlugin.Score(context.Background(), state, tt.pod, nodeName)
				assert.True(t, status.IsSuccess())
				actualList = append(actualList, framework.NodeScore{Name: nodeName, Score: score})
			}
			scorePlugin.ScoreExtensions().NormalizeScore(context.Background(), state, tt.pod, actualList)
			if tt.expectedScores != nil {
				assert.ElementsMatch(t, tt.expectedScores, actualList)
			} else {
				for _, v := range actualList {
					if v.Name == tt.expectedMapping {
						assert.Equal(t, framework.MaxNodeScore, v.Score)
					}
				}
			}
		})
	}
}

func TestBestFitPostBindAndCacheCleanup(t *testing.T) {
	b := BestFit{
		mu: sync.RWMutex{},
		scheduledPodsCache: make(map[string][]podInfo),
	}
	testNode := "node-1"
	pod1 := st.MakePod().Name("pod-1").Obj()
	pod2 := st.MakePod().Name("pod-2").Obj()
	pod3 := st.MakePod().Name("pod-3").Obj()
	b.scheduledPodsCache[testNode] = append(b.scheduledPodsCache[testNode], podInfo{pod: pod1}, podInfo{pod: pod2},
		podInfo{pod: pod3})

	pod4 := st.MakePod().Name("pod4-4").Obj()
	b.PostBind(context.Background(), framework.NewCycleState(), pod4, testNode)
	b.cleanupCache()
	assert.NotNil(t, b.scheduledPodsCache[testNode])
	assert.Equal(t, 1, len(b.scheduledPodsCache[testNode]))
	assert.Equal(t, pod4, b.scheduledPodsCache[testNode][0].pod)

	b.scheduledPodsCache[testNode] = nil
	b.scheduledPodsCache[testNode] = append(b.scheduledPodsCache[testNode], podInfo{pod: pod1}, podInfo{pod: pod2},
		podInfo{pod: pod3}, podInfo{timestamp: time.Now().Unix(), pod: pod4})
	pod5 := st.MakePod().Name("pod-5").Obj()
	b.PostBind(context.Background(), framework.NewCycleState(), pod5, testNode)
	b.cleanupCache()
	assert.NotNil(t, b.scheduledPodsCache[testNode])
	assert.Equal(t, 2, len(b.scheduledPodsCache[testNode]))
	assert.Equal(t, pod4, b.scheduledPodsCache[testNode][0].pod)
	assert.Equal(t, pod5, b.scheduledPodsCache[testNode][1].pod)

	b.scheduledPodsCache[testNode] = nil
	b.PostBind(context.Background(), framework.NewCycleState(), pod5, testNode)
	b.cleanupCache()
	assert.NotNil(t, b.scheduledPodsCache[testNode])
	assert.Equal(t, 1, len(b.scheduledPodsCache[testNode]))
	assert.Equal(t, pod5, b.scheduledPodsCache[testNode][0].pod)
}

func BenchmarkTestBestFitPlugins(b *testing.B) {
	tests := []struct {
		name     string
		podsNum  int64
		nodesNum int64
	}{
		{
			name:     "100nodes",
			podsNum:  1000,
			nodesNum: 100,
		},
		{
			name:     "1000nodes",
			podsNum:  10000,
			nodesNum: 1000,
		},
		{
			name:     "5000nodes",
			podsNum:  30000,
			nodesNum: 5000,
		},
	}

	registeredPlugins := []st.RegisterPluginFunc{
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		st.RegisterPreFilterPlugin(podtopologyspread.Name, podtopologyspread.New),
		st.RegisterFilterPlugin(podtopologyspread.Name, podtopologyspread.New),
		st.RegisterPreScorePlugin(podtopologyspread.Name, podtopologyspread.New),
		st.RegisterScorePlugin(Name, New, 1),
		st.RegisterPluginAsExtensions(Name, New, "PostBind"),
	}

	bfbpArgs := pluginConfig.BestFitBinPackArgs{}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			pod := st.MakePod().Name("p").Label("foo", "").Obj()
			state := framework.NewCycleState()

			cs := testClientSet.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			nodes := getNodes(tt.nodesNum)
			snapshot := newTestSharedLister(nil, nodes)

			nodeMetricsMap := make(map[string]watcher.NodeMetrics)
			nodeMetrics := watcher.NodeMetrics{
				Metrics: []watcher.Metrics{
					{
						Type:  watcher.CPU,
						Value: 0,
					},
				},
			}
			for _, node := range nodes {
				nodeMetricsMap[node.Name] = nodeMetrics
			}
			watcherResponse := watcher.WatcherMetrics{
				Window: watcher.Window{},
				Data: watcher.Data {
					NodeMetricsMap: nodeMetricsMap,
				},
			}

			server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
				bytes, err := json.Marshal(watcherResponse)
				if err != nil {
					klog.Fatalf("Error marshalling watcher response: %v", err)
				}
				resp.Write(bytes)
			}))
			// point watcher to test server
			WatcherHostName = server.URL
			WatcherBaseUrl = ""
			defer server.Close()

			fh, err := st.NewFramework(registeredPlugins, runtime.WithClientSet(cs),
				runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))
			assert.Nil(b, err)
			pl, err := New(&bfbpArgs, fh)
			assert.Nil(b, err)
			scorePlugin := pl.(framework.ScorePlugin)
			postBindPlugin := pl.(framework.PostBindPlugin)
			informerFactory.Start(context.Background().Done())
			informerFactory.WaitForCacheSync(context.Background().Done())

			ctx := context.Background()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				gotList := make(framework.NodeScoreList, len(nodes))
				scoreNode := func(i int) {
					n := nodes[i]
					score, _ := scorePlugin.Score(ctx, state, pod, n.Name)
					gotList[i] = framework.NodeScore{Name: n.Name, Score: score}
				}
				Until(ctx, len(nodes), scoreNode)
				status := (scorePlugin.(framework.ScoreExtensions)).NormalizeScore(ctx, state, pod, gotList)
				assert.True(b, status.IsSuccess())
				postBindPlugin.PostBind(ctx, state, pod, pod.Spec.NodeName)
			}
		})
	}
}

const parallelism = 16

// Copied from k8s internal
// chunkSizeFor returns a chunk size for the given number of items to use for
// parallel work. The size aims to produce good CPU utilization.
func chunkSizeFor(n int) workqueue.Options {
	s := int(math.Sqrt(float64(n)))
	if r := n/parallelism + 1; s > r {
		s = r
	} else if s < 1 {
		s = 1
	}
	return workqueue.WithChunkSize(s)
}

// Copied from k8s internal
// Until is a wrapper around workqueue.ParallelizeUntil to use in scheduling algorithms.
func Until(ctx context.Context, pieces int, doWorkPiece workqueue.DoWorkPieceFunc) {
	workqueue.ParallelizeUntil(ctx, parallelism, pieces, doWorkPiece, chunkSizeFor(pieces))
}

func newTestSharedLister(pods []*v1.Pod, nodes []*v1.Node) *testSharedLister {
	nodeInfoMap := make(map[string]*framework.NodeInfo)
	nodeInfos := make([]*framework.NodeInfo, 0)
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if _, ok := nodeInfoMap[nodeName]; !ok {
			nodeInfoMap[nodeName] = framework.NewNodeInfo()
		}
		nodeInfoMap[nodeName].AddPod(pod)
	}
	for _, node := range nodes {
		if _, ok := nodeInfoMap[node.Name]; !ok {
			nodeInfoMap[node.Name] = framework.NewNodeInfo()
		}
		err := nodeInfoMap[node.Name].SetNode(node)
		if err != nil {
			log.Fatal(err)
		}
	}

	for _, v := range nodeInfoMap {
		nodeInfos = append(nodeInfos, v)
	}

	return &testSharedLister{
		nodes:       nodes,
		nodeInfos:   nodeInfos,
		nodeInfoMap: nodeInfoMap,
	}
}

func getPodWithContainersAndOverhead(overhead int64, requests ...int64) *v1.Pod {
	newPod := st.MakePod()
	newPod.Spec.Overhead = make(map[v1.ResourceName]resource.Quantity)
	newPod.Spec.Overhead[v1.ResourceCPU] = *resource.NewMilliQuantity(overhead, resource.DecimalSI)


	for i := 0; i < len(requests); i++ {
		newPod.Container("test-container-" + strconv.Itoa(i))
	}
	for i, request := range requests {
		newPod.Spec.Containers[i].Resources.Requests = make(map[v1.ResourceName]resource.Quantity)
		newPod.Spec.Containers[i].Resources.Requests[v1.ResourceCPU] = *resource.NewMilliQuantity(request, resource.DecimalSI)
		newPod.Spec.Containers[i].Resources.Limits = make(map[v1.ResourceName]resource.Quantity)
		newPod.Spec.Containers[i].Resources.Limits[v1.ResourceCPU] = *resource.NewMilliQuantity(request, resource.DecimalSI)
	}
	return newPod.Obj()
}

func getNodes(nodesNum int64) (nodes []*v1.Node) {
	nodeResources := map[v1.ResourceName]string{
		v1.ResourceCPU:    "64000m",
		v1.ResourceMemory: "346Gi",
	}
	var i int64
	for i = 0; i < nodesNum; i++ {
		nodes = append(nodes, st.MakeNode().Name(fmt.Sprintf("node-%v", i)).Capacity(nodeResources).Obj())
	}
	return
}
