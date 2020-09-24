# KEP - Real Load Aware Scheduling


## Summary

Real Load Aware Scheduling minimizes cluster management costs and makes the cluster more efficient in terms of resource (CPU, Memory) utilization.


## Motivation

Clusters are inefficient due to a lack of capacity utilization, which leads to increased costs due to more machines, maintenance, etc. The main reason for this is that the default scheduler in K8s doesn’t consider live node utilization values in scheduling decisions, and pod requests are not accurate in practice.


### Goals

1. Minimize the number of running nodes.
2. CPU Utilisation per node should not go beyond X%.
3. Instances of an app should spread across failure domains. The failure domain would be host at the lowest level and can extend to racks, zones, regions, etc.
4. To not affect the behavior of default score plugins unless necessary.

### Non-Goals

1. The constraints above are the best effort of soft constraints. They are not hard constraints.
2. Descheduling or pre-emption due to lousy scheduling decisions is not addressed in the initial design.
3. Memory, Network, and Disk utilization are not considered in the initial design.

## Proposal


### User Stories


#### Story 1


As a company relying on the public cloud, we would like to minimize machine costs by efficiently utilizing the nodes leased.


#### Story 2

As a company-owned data center, we would like to minimize the hardware and maintenance associated with clusters by getting the maximum possible juice out of existing machines.


#### Story 3

As a K8s user, I would like to see more enriched single-level scheduler options in the community.


### Notes/Constraints/Caveats

Enabling our plugin(s) will cause conflict with 2 default scoring plugins: “NodeResourcesLeastAllocated” & “NodeResourcesBalancedAllocation” score plugins. So it is strongly advised to disable them.


### Risks and Mitigations

If utilization metrics are not available for a long time, we will fall back to the best fit bin pack based on allocations. There is no user action needed for this.


## Design Details


![](images/design.png "design")



### Metrics Provider

This service provides metrics backed by a time-series database — for example, Prometheus, InfluxDB, etc.


### Load Watcher/Load Analyser

The load watcher and analyzer both run in a single process. The watcher is responsible for the cluster-wide aggregation of resource usage metrics like CPU, memory, network, and IO stats over windows of a specified duration. It stores these in its local cache and persists several aggregations in the host local DB for fault tolerance. The analyzer is responsible for the detection of bad metrics and any remediation. Bad metrics could be due to missing metrics or metrics with considerable errors, making them anomalies. It can also be extended in the future to use ML models for analyzing metrics.

Load watcher would cache metrics in the last 15-minute, 10-minute, and 5-minute windows, which can be queried via REST API exposed.


### DB

This is a localhost database stored as a file. This DB aims to populate the load watcher's cache quickly if it is lost due to a crash and perform scheduling decisions without any impact on latency.


### Scheduler Plugins

This uses the scheduler framework of K8s to incorporate our custom real load aware scheduler plugins without modifying the core scheduler code.


### BestFitBinPack Plugins


#### PreScore plugin

This extension point which is invoked for all the nodes at once will be used to cache metrics for all nodes using a single REST API  call. This optimization will reduce multiple calls for each node from the Score plugin below, thereby reducing overall scheduling latency.


#### Score plugin

This plugin would extend the Score extension point. K8s scheduler framework calls the Score function for each node separately when scheduling a pod.

Following is the algorithm:

**Algorithm**

1. Get the utilization of the current node to be scored. Call it A.
2. Calculate the current pod's total CPU requests and overhead. Call it B.
3. Calculate the expected utilization if the pod is scheduled under this node by adding i.e. U = A + B.
4. If U &lt;= 50%, return U+50 as the score
5. If 50% &lt; U &lt;= 100%, return 100 - U
6. If U > 100%, return 0

For example, let’s say we have three nodes X, Y, and Z, with four cores each and utilization 1, 2, and 3 cores respectively. For simplicity, let’s assume our pod to be scheduled has 0 cores CPU requests and overhead.

Utilization of each node:


```
Ux → (1/4)*100 = 25
Uy → (2/4)*100 = 50
Uz → (3/4)*100 = 75
```


The score of each node :


```
X → 50 + 25 =  75
Y → 50 + 50 = 100
Z → 75 - 50 = 25
```


In the algorithm above, 50% is the target utilization rate we want to achieve on all nodes. We can be less aggressive by reducing it to 40% so that it has much lesser chances of going over 50% during spikes or unexpected loads. 

In the 2nd step of the algorithm, one variant uses the current pod's total CPU limits instead of requests, to have a stronger upper bound of expected utilization.

The 50% threshold value for utilisation will be made configurable.

**Algorithm Analysis**

<img src="images/bfbp-graph.png" alt="bfbp-graph" width="600" height="400"/>


The above is a plot of the piecewise function outlined in the algorithm. The key observations here are manifold:



1. As the utilization goes from 0 to 50%, we pack pods on those nodes by favoring them.
2. The nodes are penalized linearly as the utilization goes beyond 50%, to spread the pod amongst those "hot" nodes.
3. The positive slope begins from 50 and not from 0 because the range of score is from 0-100, and we want to maximize our score output for nodes we wish to favor so that the score is more substantial and other plugins do not affect it much.
4. There is a break in the graph with a high drop due to the "penalty" we have.

#### Spreading with BestFitBinPack


To meet the 2nd constraint in the problem statement, we utilize the existing PodTopologySpread (PTS) plugin provided by K8s. It was proved with experiments that using a dynamic weight-based approach is not a good idea here. However, if we choose a fixed large weight for PTS, say 100, we can meet our requirements. The problem with this approach is that we will end up downplaying the scores of other important default plugins like Image Locality, Interpod Affinity, etc. with their default weights. Also, with more plugins we plan to add in the future, managing weights will not be simple. So for BFBP to work well with PTS without modifying weights, the following extended algorithm is proposed that is called when scheduling replicas of a service, replica-set, replication controller, etc.


**Algorithm**



1. For each node, calculate S = (1000*PTS + PTS*BFBP)
2. Scale each S to be between 0-100
3. Return the scores

**Algorithm Analysis**

1. The algorithm favors nodes with high PTS scores with 1000*PTS part. The weight 1000 is chosen to give preference to PTS  and to avoid the congestion of scores upon scaling down, my magnifying the scores 
2. PTS*BFBP penalizes hot nodes with low BFBP scores.
3. The final scaled-down value deals with congestion, hot node penalty, and favoring of high PTS score nodes

Example: 


| Node | PTS | BFBP | Combined Score |
|------|-----|------|----------------|
|  N1  | 100 |  35  |       96       |
|  N2  |  67 |  75  |       67       |
|  N3  | 100 |  80  |       100      |
|  N4  |  82 |  90  |       83       |
|  N5  |  0  |  90  |        0       |



### Safe Balancing Plugin


#### PreScore plugin

This extension point, which is invoked for all the nodes at once, will be used to cache metrics for all nodes using a single REST API  call. This optimization will reduce multiple calls for each node from the Score plugin below, reducing overall scheduling latency.


#### Score plugin

This plugin would extend the Score extension point. K8s scheduler framework calls the Score function for each node separately when scheduling a pod.


**Idea**


Balancing load based on average would be risky sometimes, as it does not consider the bursty variations. A safe balancing plugin balances not only the average load but also the risk caused by load variations. Suppose we take the mean (M) and standard deviation (V) of all nodes’ utilization into a mu-sigma plot below. In that case, the safe balancing plugin will make placements such that all nodes’ utilization are aligned on the diagonal line, which is V + M = c. Here, c is a constant indicating the overall cluster utilization average plus the standard deviation.

<img src="images/safe-sched-graph.png" alt="safe-sched-graph" width="700" height="400"/>


Following is the algorithm:


**Algorithm**


1. Get the requested resource for the pod to be scheduled as, <img src="https://render.githubusercontent.com/render/math?math=r">.
2. Get the sliding window average (<img src="https://render.githubusercontent.com/render/math?math=M">) and standard deviation (<img src="https://render.githubusercontent.com/render/math?math=V">) of resource utilization fraction (range from 0 to 1) for all types of resources (CPU, Memory, GPU, etc.) of the current node to be scored. 
3. Calculate the score of the current node for each type of resource: <img src="https://render.githubusercontent.com/render/math?math=S_i = M %2B r %2B V">
4. Get a score for each type of resource and bound it to [0,1]: <img src="https://render.githubusercontent.com/render/math?math=S_i = \min(S_i, 1.0)">
5. Calculate the node priority score per resource as: <img src="https://render.githubusercontent.com/render/math?math=U_i = (1 - S_i) \times MaxPriority ">
6. Get the final node score as: <img src="https://render.githubusercontent.com/render/math?math=U = \min_i(U_i)">

**Example**


For example, let's say we have three nodes `N1`, `N2`, and `N3`, and the pod to be scheduled have CPU and Memory requests as 500 milicores and 1 GB. All nodes have a capacity of 4 cores and 8 GB.

The pod request fraction can be computed as <img src="https://render.githubusercontent.com/render/math?math=r_{cpu} = \frac{1}{8}, r_{memory} = \frac{1}{8}">. 


<img src="images/image6.png" alt="node-capacity" width="700" height="200"/>


Then according to steps 2 ~ 4, the mean and standard deviation of CPU and memory fraction utilization can be computed as follows:


<img src="images/image9.png" alt="node-mn-std-util" width="600" height="100"/>


The score for each type of resource and each node are as follows according to step 5 ~ 6:


<img src="images/image1.png" alt="node-mn-std-util" width="600" height="200"/>


According to the scores we have, node `N3` will be selected. The utilization fraction of nodes before and after the placement is as follows.

<img src="images/image5.png" alt="before-placement" width="600" height="150"/>

<img src="images/image7.png" alt="after-placement" width="600" height="150"/>


If we plot these in mu-sigma plots, we can see the placement automatically pushes the utilization of nodes toward the diagonal line sigma = 1 - mu.


<img src="images/image2.png" alt="mu-sigma-plot" width="580" height="600"/>



### **Bad Metrics**

Since load watcher is a major component needed by the plugin to find out utilization values, metrics are essential for the plugin’s correctness. Bad metrics can be due to several issues. A few of them being are given below, along with their remediation. Detection of bad metrics is a problem in its own, other than its remediation.

1. Unavailability of metrics: Metrics can be unavailable due to: 
    1. Short inter-arrival times of pods: In this case, we predict utilization for the recent pods that got scheduled on the node based on its request values and a multiplier, and add it to the current utilization. If the pods belong to best-effort QoS, i.e. don't have requests, we assume a number like 1 milicore which is configurable.
    2. Failure of Metric reporting agent running in node: In this case, the metrics would be missing for a particular time. We can use the lastest available utilization window (not older than 5 minutes) along with predicted utilization of pods scheduled on that node after the window end time.
    3. Metrics Provider failure: The metrics provider can fail to serve requests for several reasons like OOM, unresponsive components, etc. In this case, every node’s metrics are unavailable, and we fall back to the best fit based on allocations. 
    4. Addition of new nodes: It is easy to handle this if no pods have been scheduled on them, so utilization must be 0. If any recent new pods are running already then we can use the method above for prediction. If there are long-running pods, we can avoid this node until metrics are available.
2. Incorrect metrics (due to misconfiguration, bugs, etc.): It is challenging to detect such metrics. For the first implementation, we would like to ignore this case by assuming metrics are correct. However, we plan to add metrics from multiple providers to corroborate data points and minimize the effect of incorrect metrics on scheduling in the future. 

### **Load Watcher API**


JSON Schema - see Appendix

JSON payload example:


```
{
  "timestamp": 1556987522,
  "window": {
    "duration" : "15m",
    "start" : 1556984522,
    "end"   : 1556985422
  },
  "source" : "InfluxDB",
  "data" :
    {
      "node-1" : {
        "metrics" : [
          {
            "name" : "host.cpu.utilisation",
            "type" : "cpu",
            "rollup" : "AVG",
            "value"  : 20
          },
          {
            "name" : "host.memory.utilisation",
            "type" : "memory",
            "rollup" : "STD",
            "value"  : 5
          }],
        "tags" : [{}],
        "metadata" : {
          "dataCenter" : "data-center-1",
          "pool" : "critical-apps"
        }
      },
      "node-2" : {
        "metrics" : [
          {
            "name" : "host.cpu.utilisation",
            "type"  : "cpu",
            "rollup" : "AVG",
            "value"  : 20
          },
          {
            "name" : "host.memory.utilisation",
            "type" : "memory",
            "rollup" : "STD",
            "value"  : 5
          }
        ]
      },
      "metadata" : {
        "dataCenter" : "data-center-2",
        "pool" : "light-apps"
      },
      "tags" : [{}]
    }
}
```


**REST API**


```
GET /watcher
Returns metrics for all nodes in the cluster


GET /watcher/{hostname}
Returns metrics for the hostname given


200 OK response example given above
404 if no metrics are present
```



### **PostBind Plugin**

A custom plugin will be added at post bind extension point, which maintains a time ordered state of node → pod mappings for pods scheduled successfully in last 5 minutes. This state will be maintained across scheduling cycles and used from BestFitBinPack Score plugin. This state will help to predict utilisation based on allocations when metrics are bad, especially in case 1 above (Unavailability of metrics).


### **Test Plan**

Unit tests and Integration tests will be added.


## **Production Readiness Review Questionnaire**


### **Scalability**



*   Will enabling / using this feature result in any new API calls? 

    No.

*   Will enabling / using this feature result in introducing new API types? 

    No.

*   Will enabling / using this feature result in any new calls to the cloud provider? 

    No.

*   Will enabling / using this feature result in increasing size or count of the existing API objects? 

    No.

*   Will enabling / using this feature result in increasing time taken by any operations covered by [existing SLIs/SLOs](https://git.k8s.io/community/sig-scalability/slos/slos.md#kubernetes-slisslos)? 

    It can affect scheduler latency, however since 2 plugins need to be disabled the overall latency would be likely lesser than before. This will be confirmed with some benchmarking.

*   Will enabling / using this feature result in non-negligible increase of resource usage (CPU, RAM, disk, IO, ...) in any components? 

    No - the metrics are cached at load watcher and our plugins will only pull them when needed, and this shouldn’t be a non-negligible increase in resource usage. Moreover, the algorithms provided run in linear time for number of nodes.


### **Troubleshooting**

*   How does this feature react if the API server and/or etcd is unavailable? 

    Running pods are not affected. Any new submissions would be rejected by scheduler.

*   What are other known failure modes?

     N/A

*   What steps should be taken if SLOs are not being met to determine the problem?

    N/A


## **Implementation History**



## **Appendix**

Load Watcher JSON schema


```
{
  "$schema": "http://json-schema.org/draft-04/schema#",
  "type": "object",
  "properties": {
    "timestamp": {
      "type": "integer"
    },
    "window": {
      "type": "object",
      "properties": {
        "duration": {
          "type": "string"
        },
        "start": {
          "type": "integer"
        },
        "end": {
          "type": "integer"
        }
      },
      "required": [
        "duration",
        "start",
        "end"
      ]
    },
    "source": {
      "type": "string"
    },
    "data": {
      "type": "object",
      "patternProperties": {
        "^[0-9]+$": {
          "type": "object",
          "properties": {
            "metrics": {
              "type": "array",
              "items": [
                {
                  "type": "object",
                  "properties": {
                    "name": {
                      "type": "string"
                    },
                    "type": {
                      "type": "string"
                    },
                    "rollup": {
                      "type": "string"
                    },
                    "value": {
                      "type": "integer"
                    }
                  },
                  "required": [
                    "name",
                    "type",
                    "rollup",
                    "value"
                  ]
                },
                {
                  "type": "object",
                  "properties": {
                    "name": {
                      "type": "string"
                    },
                    "type": {
                      "type": "string"
                    },
                    "rollup": {
                      "type": "string"
                    },
                    "value": {
                      "type": "integer"
                    }
                  },
                  "required": [
                    "name",
                    "type",
                    "rollup",
                    "value"
                  ]
                }
              ]
            },
            "tags": {
              "type": "array",
              "items": [
                {
                  "type": "object"
                }
              ]
            },
            "metadata": {
              "type": "object",
              "properties": {
                "dataCenter": {
                  "type": "string"
                },
                "pool": {
                  "type": "string"
                }
              }
            }
          },
          "required": [
            "metrics"
          ]
        }
      }
    }
  },
  "required": [
    "timestamp",
    "window",
    "source",
    "data"
  ]
}
