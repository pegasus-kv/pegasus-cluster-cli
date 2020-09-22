package pegasus

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Deployment interface {
	StartNode(Node) error
	StopNode(Node) error
	RollingUpdate(Node) error
	ListAllNodes() ([]Node, error)
}

type DeployError struct {
	Msg    string
	Output []byte
}

func (e *DeployError) Error() string {
	return e.Msg + ". Output:\n" + string(e.Output)
}

func NewDeployError(msg string, out []byte) *DeployError {
	return &DeployError{msg, out}
}

var CreateDeployment func(cluster string) Deployment = nil

func ValidateCluster(cluster string, metaList string, nodeNames []string) (string, error) {
	fmt.Println("Validate cluster name and node list...")
	nodeMap := make(map[string]bool)
	for _, name := range nodeNames {
		_, prs := nodeMap[name]
		if prs {
			return "", errors.New("duplicate node '" + name + "' in node list")
		}
		nodeMap[name] = true
	}

	ok1, ok2 := false, false
	r1 := regexp.MustCompile(`/([^/]*)$`)
	r2 := regexp.MustCompile(`([0-9.:]*)\s*$`)
	cmd, err := runShellInput("cluster_info", metaList)
	if err != nil {
		return "", err
	}

	var pmeta string
	out, err := checkOutput(cmd, true, func(line string) bool {
		if strings.Contains(line, "zookeeper_root") {
			rs := r1.FindStringSubmatch(line)
			if len(rs) > 1 && strings.TrimSpace(rs[1]) == cluster {
				ok1 = true
			}
		} else if strings.Contains(line, "primary_meta_server") {
			rs := r2.FindStringSubmatch(line)
			if len(rs) > 1 && len(rs[1]) != 0 {
				ok2 = true
				pmeta = rs[1]
			}
		}
		return ok1 && ok2
	})
	if err != nil {
		return "", err
	}
	if !ok1 {
		return "", NewDeployError("cluster name and meta list not matched", out)
	} else if !ok2 {
		return "", NewDeployError("extract primary_meta_server by shell failed", out)
	} else {
		return pmeta, nil
	}
}

func AddNodes(cluster string, deploy Deployment, metaList string, nodeNames []string) error {
	if err := initNodes(deploy); err != nil {
		return err
	}
	pmeta, err := ValidateCluster(cluster, metaList, nodeNames)
	if err != nil {
		return err
	}

	if err = setMetaLevel("steady", metaList); err != nil {
		return err
	}

	fmt.Println()
	for _, name := range nodeNames {
		node, ok := findReplicaNode(name)
		if !ok {
			return errors.New("replica node '" + name + "' not found")
		}
		fmt.Println("Starting node " + node.IPPort + " by deployment...")
		if err := deploy.StartNode(node); err != nil {
			return err
		}
		fmt.Println("Starting node by deployment done")
		fmt.Println()
	}
	if err := rebalanceCluster(pmeta, metaList, false); err != nil {
		return err
	}

	return nil
}

func RemoveNodes(cluster string, deploy Deployment, metaList string, nodeNames []string) error {
	if err := initNodes(deploy); err != nil {
		return err
	}
	pmeta, err := ValidateCluster(cluster, metaList, nodeNames)
	if err != nil {
		return err
	}

	nodes := make([]Node, len(nodeNames))
	addrs := make([]string, len(nodeNames))
	for i, name := range nodeNames {
		node, ok := findReplicaNode(name)
		if !ok {
			return errors.New("replica node '" + name + "' not found")
		}
		nodes[i] = node
		addrs[i] = node.IPPort
	}
	if err := setRemoteCommand(pmeta, "meta.lb.assign_secondary_black_list", strings.Join(addrs, ","), metaList, "set ok"); err != nil {
		return err
	}
	if err := setRemoteCommand(pmeta, "meta.live_percentage", "0", metaList, "OK"); err != nil {
		return err
	}

	fmt.Println()
	for _, node := range nodes {
		if err := removeNode(deploy, metaList, pmeta, node); err != nil {
			return err
		}
		fmt.Println()
	}
	return nil
}

func RollingUpdateNodes(cluster string, deploy Deployment, metaList string, nodeNames []string) error {
	if err := initNodes(deploy); err != nil {
		return err
	}
	pmeta, err := ValidateCluster(cluster, metaList, nodeNames)
	if err != nil {
		return err
	}

	if err := setMetaLevel("steady", metaList); err != nil {
		return err
	}

	fmt.Println()
	if nodeNames == nil {
		for _, node := range globalAllNodes {
			if node.Job == JobReplica {
				if err := rollingUpdateNode(deploy, pmeta, metaList, node); err != nil {
					return err
				}
				fmt.Println()
			}
		}
	} else {
		for _, name := range nodeNames {
			node, ok := findReplicaNode(name)
			if !ok {
				return errors.New("replica node '" + name + "' not found")
			}
			if err := rollingUpdateNode(deploy, pmeta, metaList, node); err != nil {
				return err
			}
			fmt.Println()
		}
	}

	if err := setRemoteCommand(pmeta, "meta.lb.add_secondary_max_count_for_one_node", "DEFAULT", metaList, "OK"); err != nil {
		return err
	}
	if nodeNames == nil {
		fmt.Println("Rolling update meta servers...")
		for _, node := range globalAllNodes {
			if node.Job == JobMeta {
				if err := deploy.RollingUpdate(node); err != nil {
					return err
				}
			}
		}
		fmt.Println("Rolling update meta servers done")
		fmt.Println("Rolling update collectors...")
		for _, node := range globalAllNodes {
			if node.Job == JobCollector {
				if err := deploy.RollingUpdate(node); err != nil {
					return err
				}
			}
		}
		fmt.Println("Rolling update collectors done")

		rebalanceCluster(pmeta, metaList, false)
	}

	return nil
}

func rollingUpdateNode(deploy Deployment, pmeta string, metaList string, node Node) error {
	fmt.Printf("Rolling update replica server %s of %s...\n", node.Name, node.IPPort)

	if err := setRemoteCommand(pmeta, "meta.lb.add_secondary_max_count_for_one_node", "0", metaList, "OK"); err != nil {
		return err
	}

	c := 0
	var gpids []string
	fmt.Println("Migrating primary replicas out of node...")
	fin, err := waitFor(func() (bool, error) {
		if c%10 == 0 {
			if err := runSh("migrate_node", "-c", metaList, "-n", node.IPPort, "-t", "run").Start(); err != nil {
				return false, err
			}
			fmt.Println("Sent migrate propose")
		}
		cmd, err := runShellInput("nodes -d", metaList)
		if err != nil {
			return false, err
		}
		priCount := -1
		if _, err := checkOutput(cmd, false, func(line string) bool {
			if strings.Contains(line, node.IPPort) {
				ss := strings.Fields(line)
				if len(ss) > 3 {
					res, err := strconv.Atoi(ss[3])
					if err != nil {
						return false
					}
					c = res
					return true
				}
			}
			priCount = 0
			return false
		}); err != nil {
			return false, err
		}
		fmt.Println("Still " + strconv.Itoa(priCount) + " primary replicas left on " + node.IPPort)
		c++
		return priCount == 0, nil
	}, time.Second, 28)
	if err != nil {
		return err
	}
	if fin {
		fmt.Println("Migrate done")
	} else {
		fmt.Println("Migrate timeout")
	}
	time.Sleep(time.Second)

	fmt.Println("Downgrading replicas on node...")
	c = 0
	fin, err = waitFor(func() (bool, error) {
		if c%10 == 0 {
			gpids = []string{}
			if _, err := checkOutput(runSh("downgrade_node", "-c", metaList, "-n", node.IPPort, "-t", "run"), false, func(line string) bool {
				if strings.HasPrefix(line, "propose ") {
					gpids = append(gpids, strings.ReplaceAll(strings.Fields(line)[2], ".", " "))
				}
				return false
			}); err != nil {
				return false, err
			}
			fmt.Println("Sent downgrade propose")
		}
		cmd, err := runShellInput("nodes -d", metaList)
		if err != nil {
			return false, err
		}
		priCount := -1
		if _, err := checkOutput(cmd, false, func(line string) bool {
			if strings.Contains(line, node.IPPort) {
				ss := strings.Fields(line)
				res, err := strconv.Atoi(ss[3])
				if err != nil {
					return false
				}
				c = res
				return true
			}
			priCount = 0
			return false
		}); err != nil {
			return false, err
		}
		fmt.Println("Still " + strconv.Itoa(priCount) + " primary replicas left on " + node.IPPort)
		c++
		return priCount == 0, nil
	}, time.Second, 28)
	if err != nil {
		return err
	}
	if fin {
		fmt.Println("Downgrade done")
	} else {
		fmt.Println("Downgrade timeout")
	}
	time.Sleep(time.Second)

	c = 0
	r1 := regexp.MustCompile(`replica_stub.replica\(Count\)","type":"NUMBER","value":([0-9]*)`)
	r2 := regexp.MustCompile(`replica_stub.opening.replica\(Count\)","type":"NUMBER","value":([0-9]*)`)
	r3 := regexp.MustCompile(`replica_stub.closing.replica\(Count\)","type":"NUMBER","value":([0-9]*)`)
	fmt.Println("Checking replicas closed on node...")
	fin, err = waitFor(func() (bool, error) {
		if c%10 == 0 {
			if err := killPartitions(gpids, node, metaList); err != nil {
				return false, err
			}
		}
		cmd, err := runShellInput("remote_command -l "+node.IPPort+" perf-counters '.*replica(Count)'", metaList)
		if err != nil {
			return false, err
		}
		serving := -1
		opening := -1
		closing := -1
		out, err := checkOutput(cmd, true, func(line string) bool {
			ss := r1.FindStringSubmatch(line)
			if len(ss) > 1 {
				if v, err := strconv.Atoi(ss[1]); err == nil {
					serving = v
				}
			}
			ss = r2.FindStringSubmatch(line)
			if len(ss) > 1 {
				if v, err := strconv.Atoi(ss[1]); err == nil {
					opening = v
				}
			}
			ss = r3.FindStringSubmatch(line)
			if len(ss) > 1 {
				if v, err := strconv.Atoi(ss[1]); err == nil {
					closing = v
				}
			}
			return serving != -1 && opening != -1 && closing != -1
		})
		if err != nil {
			return false, err
		}
		if serving == -1 || opening == -1 || closing == -1 {
			return false, NewDeployError("extract replica count from perf counters failed", out)
		}
		fmt.Println("Still " + strconv.Itoa(serving+opening+closing) + " replicas not closed on " + node.IPPort)
		c++
		return serving+opening+closing == 0, nil
	}, time.Second, 28)
	if err != nil {
		return err
	}
	if fin {
		fmt.Println("Close done")
	} else {
		fmt.Println("Close timeout")
	}

	if err := startRunShellInput("remote_command -l "+node.IPPort+" flush_log", metaList); err != nil {
		return err
	}

	if err := setRemoteCommand(pmeta, "meta.lb.add_secondary_max_count_for_one_node", "100", metaList, "OK"); err != nil {
		return err
	}

	fmt.Println("Rolling update by deployment...")
	if err := deploy.RollingUpdate(node); err != nil {
		return err
	}
	fmt.Println("Rolling update by deployment done")

	fmt.Println("Wait " + node.IPPort + " to become alive...")
	if _, err := waitFor(func() (bool, error) {
		cmd, err := runShellInput("nodes -d", metaList)
		if err != nil {
			return false, err
		}
		var status string
		if _, err := checkOutput(cmd, false, func(line string) bool {
			if strings.Contains(line, node.IPPort) {
				ss := strings.Fields(line)
				if len(ss) < 2 {
					return false
				}
				status = ss[1]
			}
			return false
		}); err != nil {
			return false, err
		}
		return status == "ALIVE", nil
	}, time.Second, 0); err != nil {
		return err
	}

	fmt.Println("Wait " + node.IPPort + " to become healthy...")
	if err := waitForHealthy(metaList); err != nil {
		return err
	}

	return nil
}

func removeNode(deploy Deployment, metaList string, pmeta string, node Node) error {
	fmt.Println("Stopping replica node " + node.Name + " of " + node.IPPort + " ...")
	if err := setMetaLevel("steady", metaList); err != nil {
		return err
	}

	if err := setRemoteCommand(pmeta, "meta.lb.assign_delay_ms", "10", metaList, "OK"); err != nil {
		return err
	}

	// migrate node
	fmt.Println("Migrating primary replicas out of node...")
	if err := runSh("migrate_node", "-c", metaList, "-n", node.IPPort, "-t", "run").Run(); err != nil {
		return err
	}
	// wait for pri_count == 0
	fmt.Println("Wait " + node.IPPort + " to migrate done...")
	if _, err := waitFor(func() (bool, error) {
		val := 0
		cmd, err := runShellInput("nodes -d", metaList)
		if err != nil {
			return false, err
		}
		if _, err := checkOutput(cmd, false, func(line string) bool {
			if strings.Contains(line, node.IPPort) {
				ss := strings.Fields(line)
				if len(ss) > 3 {
					val, err = strconv.Atoi(ss[3])
					if err != nil {
						return false
					}
					return true
				}
			}
			return false
		}); err != nil {
			return false, err
		}
		if val == 0 {
			fmt.Println("Migrate done.")
			return true, nil
		} else {
			fmt.Println("Still " + strconv.Itoa(val) + " primary replicas left on " + node.IPPort)
			return false, nil
		}
	}, time.Second, 0); err != nil {
		return err
	}
	time.Sleep(time.Second)

	// downgrade node and kill partition
	fmt.Println("Downgrading replicas on node...")
	var gpids []string
	if _, err := checkOutput(runSh("downgrade_node", "-c", metaList, "-n", node.IPPort, "-t", "run"), false, func(line string) bool {
		if strings.HasPrefix(line, "propose ") {
			ss := strings.Fields(line)
			if len(ss) > 2 {
				gpids = append(gpids, strings.ReplaceAll(ss[2], ".", " "))
			}
			return true
		}
		return false
	}); err != nil {
		return err
	}
	// wait for rep_count == 0
	fmt.Println("Wait " + node.IPPort + " to downgrade done...")
	if _, err := waitFor(func() (bool, error) {
		val := 0
		cmd, err := runShellInput("nodes -d", metaList)
		if err != nil {
			return false, err
		}
		if _, err := checkOutput(cmd, false, func(line string) bool {
			if strings.Contains(line, node.IPPort) {
				val, err = strconv.Atoi(strings.Fields(line)[2])
				if err != nil {
					return false
				}
				return true
			}
			return false
		}); err != nil {
			return false, err
		}
		if val == 0 {
			fmt.Println("Downgrade done.")
			return true, nil
		} else {
			fmt.Println("Still " + strconv.Itoa(val) + " replicas left on " + node.IPPort)
			return false, nil
		}
	}, time.Second, 0); err != nil {
		return err
	}
	time.Sleep(time.Second)

	if err := killPartitions(gpids, node, metaList); err != nil {
		return err
	}

	fmt.Println("Stop node by deployment...")
	if err := deploy.StopNode(node); err != nil {
		return err
	}
	fmt.Println("Stop node by deployment done")
	time.Sleep(time.Second)

	if err := waitForHealthy(metaList); err != nil {
		return err
	}

	if err := setRemoteCommand(pmeta, "meta.lb.assign_delay_ms", "DEFAULT", metaList, "OK"); err != nil {
		return err
	}
	return nil
}

func rebalanceCluster(pmeta string, metaList string, primaryOnly bool) error {
	if primaryOnly {
		if err := setRemoteCommand(pmeta, "meta.lb.only_move_primary", "true", metaList, "OK"); err != nil {
			return err
		}
	}

	if err := setMetaLevel("lively", metaList); err != nil {
		return err
	}

	fmt.Println("Wait for 3 minutes to do load balance...")
	time.Sleep(time.Duration(180) * time.Second)

	remainTimes := 1
	r := regexp.MustCompile(`total=(\d+)`)
	for {
		cmd, err := runShellInput("cluster_info", metaList)
		if err != nil {
			return err
		}
		var opCount string
		_, err = checkOutput(cmd, false, func(line string) bool {
			if strings.Contains(line, "balance_operation_count") {
				rs := r.FindStringSubmatch(line)
				if len(rs) > 1 {
					opCount = rs[1]
					return true
				}
			}
			return false
		})
		if opCount == "" {
			break
		}

		if opCount == "0" {
			if remainTimes == 0 {
				break
			} else {
				fmt.Println("cluster may be balanced, try wait 30 seconds...")
				remainTimes--
				time.Sleep(time.Duration(30) * time.Second)
			}
		} else {
			fmt.Println("still " + opCount + " balance operations to do...")
			time.Sleep(time.Duration(10) * time.Second)
		}
	}

	if err := setMetaLevel("steady", metaList); err != nil {
		return err
	}

	if primaryOnly {
		if err := setRemoteCommand(pmeta, "meta.lb.only_move_primary", "false", metaList, "OK"); err != nil {
			return err
		}
	}
	return nil
}
