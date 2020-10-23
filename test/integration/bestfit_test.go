package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/podtopologyspread"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/selectorspread"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutils "k8s.io/kubernetes/test/integration/util"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran/binpack/bestfit"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestBestFitBinPackPlugin(t *testing.T) {
	registry := fwkruntime.Registry{bestfit.Name: bestfit.New}

	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal([]byte{})
		assert.Nil(t, err)
		resp.Write(bytes)
	}))
	// point watcher to test server
	bestfit.WatcherHostName = server.URL
	bestfit.WatcherBaseUrl = ""

	defer server.Close()
	profile := schedapi.KubeSchedulerProfile{
		SchedulerName: v1.DefaultSchedulerName,
		Plugins: &schedapi.Plugins{
			Score: &schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: bestfit.Name},
				},
				Disabled: []schedapi.Plugin{
					{Name: noderesources.LeastAllocatedName},
					{Name: noderesources.BalancedAllocationName},
					{Name: podtopologyspread.Name},
					{Name: selectorspread.Name},
				},
			},
			PostBind: &schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: bestfit.Name},
				},
			},
		},
		PluginConfig: []schedapi.PluginConfig{
			{Name: bestfit.Name, Args: &config.BestFitBinPackArgs{
				PTSArgs: schedapi.PodTopologySpreadArgs{},
			}},
		},
	}

	testCtx := util.InitTestSchedulerWithOptions(
		t,
		testutils.InitTestMaster(t, "sched-bestfit", nil),
		true,
		scheduler.WithProfiles(profile),
		scheduler.WithFrameworkOutOfTreeRegistry(registry),
	)

	defer testutils.CleanupTest(t, testCtx)

	cs, ns := testCtx.ClientSet, testCtx.NS.Name

	var nodes []*v1.Node
	nodeNames := []string{"node-1", "node-2", "node-3"}
	for i := 0; i < len(nodeNames); i++ {
		node := st.MakeNode().Name(nodeNames[i]).Label("node", nodeNames[i]).Obj()
		node.Status.Allocatable = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceCPU: 	*resource.NewQuantity(2, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(256, resource.DecimalSI),
		}
		node.Status.Capacity = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceCPU: 	*resource.NewQuantity(2, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(256, resource.DecimalSI),
		}

		node, err := cs.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
		assert.Nil(t, err)
		nodes = append(nodes, node)
	}

	var existingPods []*v1.Pod
	podNames := []string{"pod-1", "pod-2", "pod-3"}
	assignedNodeNames := []string{nodeNames[0], nodeNames[0], nodeNames[1]}
	for i := 0; i < len(podNames); i++ {
		pod := st.MakePod().Namespace(ns).Name(podNames[i]).Container(imageutils.GetPauseImageName()).Obj()
		pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU: *resource.NewMilliQuantity(200, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(50, resource.DecimalSI),
			},
		}
		pod.Spec.NodeName = assignedNodeNames[i]
		existingPods = append(existingPods, pod)
	}

	var newPods []*v1.Pod
	podNames = []string{"pod-4", "pod-5"}
	containerCPU := []int64{400, 100}
	for i := 0; i < len(podNames); i++ {
		pod := st.MakePod().Namespace(ns).Name(podNames[i]).Container(imageutils.GetPauseImageName()).Obj()
		pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU: *resource.NewMilliQuantity(containerCPU[i], resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(50, resource.DecimalSI),
			},
		}
		newPods = append(newPods, pod)
	}

	expected := [2]string{nodeNames[0], nodeNames[1]}
	for i := range newPods {
		// Wait for the pod to be scheduled.
		err := wait.Poll(1*time.Second, 120*time.Second, func() (bool, error) {
			return podScheduled(cs, newPods[i].Namespace, newPods[i].Name), nil
		})
		assert.Nil(t, err)

		pod, err := cs.CoreV1().Pods(ns).Get(context.TODO(), newPods[i].Name, metav1.GetOptions{})
		assert.Nil(t, err)

		assert.Equal(t, expected[i], pod.Spec.NodeName)
	}

}
