/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package pegasus

import (
	"strconv"
	"strings"
)

type ClusterInfo struct {
	Cluster               string
	PrimaryMeta           string
	BalanceOperationCount int
}

type HealthyInfo struct {
	PartitionCount int
	FullyHealthy   int
	Unhealthy      int
	WriteUnhealthy int
	ReadUnhealthy  int
}

type JobType int

const (
	JobMeta      = 0
	JobReplica   = 1
	JobCollector = 2
)

func (j JobType) String() string {
	switch j {
	case JobMeta:
		return "meta"
	case JobReplica:
		return "replica"
	default:
		return "collector"
	}
}

type Node struct {
	Job    JobType
	Name   string
	IPPort string
	Info   *NodeInfo
}

type NodeInfo struct {
	Status         string
	ReplicaCount   int
	PrimaryCount   int
	SecondaryCount int
}

var globalAllNodes []Node

func listAndCacheAllNodes(deploy Deployment) error {
	res, err := deploy.ListAllNodes()
	if err != nil {
		return err
	}
	globalAllNodes = res
	return nil
}

func findReplicaNode(name string) (Node, bool) {
	for _, node := range globalAllNodes {
		if node.Job == JobReplica && name == node.Name {
			return node, true
		}
	}
	return Node{}, false
}

func GetClusterInfo(metaList string) (*ClusterInfo, error) {
	cmd, err := runShellInput("cluster_info", metaList)
	if err != nil {
		return nil, err
	}
	var (
		primaryMeta *string
		clusterName *string
		opCount     *int
	)
	out, err := checkOutput(cmd, true, func(line string) bool {
		if strings.HasPrefix(line, "primary_meta_server") {
			ss := strings.Fields(line)
			if len(ss) > 2 {
				primaryMeta = &ss[2]
			}
		} else if strings.HasPrefix(line, "zookeeper_root") {
			ss := strings.Fields(line)
			if len(ss) > 2 {
				ss1 := strings.Split(ss[2], "/")
				clusterName = &ss1[len(ss1)-1]
			}
		} else if strings.HasPrefix(line, "balance_operation_count") {
			ss := strings.Fields(line)
			if len(ss) > 2 {
				s := ss[2]
				i := strings.LastIndexByte(s, '=')
				if i != -1 {
					n, err := strconv.Atoi(s[i+1:])
					if err == nil {
						opCount = &n
					}
				}
			}
		}
		return primaryMeta != nil && clusterName != nil && opCount != nil
	})
	if err != nil {
		return nil, err
	}
	if primaryMeta == nil || clusterName == nil || opCount == nil {
		return nil, NewCommandError("failed to get cluster info", out)
	}
	return &ClusterInfo{
		Cluster:               *clusterName,
		PrimaryMeta:           *primaryMeta,
		BalanceOperationCount: *opCount,
	}, nil
}
