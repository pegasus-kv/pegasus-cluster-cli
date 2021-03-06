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
	"github.com/pegasus-kv/cluster-cli/deployment"
	log "github.com/sirupsen/logrus"
)

// AddNodes implements the add-node command.
func AddNodes(cluster string, deploy deployment.Deployment, nodeNames []string) error {
	meta, err := newMeta(cluster, deploy)
	if err != nil {
		return err
	}

	if err = meta.SetMetaLevelSteady(); err != nil {
		return err
	}

	for _, name := range nodeNames {
		node, err := findReplicaNode(name)
		if err != nil {
			return err
		}
		log.Printf("Starting node %s by deployment...", node.IPPort)
		if err := deploy.StartNode(*node); err != nil {
			return err
		}
		log.Print("Starting node by deployment done")
	}

	if err := meta.Rebalance(false); err != nil {
		return err
	}

	return nil
}
