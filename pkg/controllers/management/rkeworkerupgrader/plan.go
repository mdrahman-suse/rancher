package rkeworkerupgrader

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/pkg/errors"
	"github.com/rancher/rancher/pkg/controllers/management/clusterprovisioner"
	kd "github.com/rancher/rancher/pkg/controllers/management/kontainerdrivermetadata"
	"github.com/rancher/rancher/pkg/controllers/management/secretmigrator/assemblers"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/librke"
	nodeserver "github.com/rancher/rancher/pkg/rkenodeconfigserver"
	rkeservices "github.com/rancher/rke/services"
	rketypes "github.com/rancher/rke/types"
	"github.com/sirupsen/logrus"
)

const (
	restoreKey     = "CATTLE_ETCD_RESTORE_GENERATION"
	kubeletProcess = "kubelet"
)

func (uh *upgradeHandler) nonWorkerPlan(node *v3.Node, cluster *v3.Cluster) (*rketypes.RKEConfigNodePlan, error) {
	appliedSpec, err := assemblers.AssembleRKEConfigSpec(cluster, *cluster.Status.AppliedSpec.DeepCopy(), uh.secretLister)
	if err != nil {
		return nil, err
	}
	rkeConfig := appliedSpec.RancherKubernetesEngineConfig.DeepCopy()
	rkeConfig.Nodes = []rketypes.RKEConfigNode{
		*node.Status.NodeConfig,
	}
	rkeConfig.Nodes[0].Role = []string{rkeservices.WorkerRole, rkeservices.ETCDRole, rkeservices.ControlRole}

	infos, err := librke.GetDockerInfo(node)
	if err != nil {
		return nil, err
	}

	hostAddress := node.Status.NodeConfig.Address
	hostDockerInfo := infos[hostAddress]

	logrus.Debugf("getDockerInfo for node [%s] dockerInfo [%s]", node.Name, hostDockerInfo.DockerRootDir)

	svcOptions, err := uh.getServiceOptions(rkeConfig.Version, hostDockerInfo.OSType)
	if err != nil {
		return nil, err
	}

	plan, err := librke.New().GeneratePlan(uh.ctx, rkeConfig, infos, svcOptions)
	if err != nil {
		return nil, err
	}

	token, err := uh.systemAccountManager.GetOrCreateSystemClusterToken(cluster.Name)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create or get cluster token for share-mnt")
	}

	np := &rketypes.RKEConfigNodePlan{}

	for _, tempNode := range plan.Nodes {
		if tempNode.Address == hostAddress {

			np.Processes, err = nodeserver.AugmentProcesses(token, tempNode.Processes, false,
				node.Status.NodeConfig.HostnameOverride, cluster, uh.secretLister)
			if err != nil {
				return np, err
			}

			np.Processes = nodeserver.AppendTaintsToKubeletArgs(np.Processes, node.Status.NodeConfig.Taints)

			return np, nil
		}
	}

	return nil, fmt.Errorf("failed to find plan for %s", hostAddress)
}

func (uh *upgradeHandler) workerPlan(node *v3.Node, cluster *v3.Cluster) (*rketypes.RKEConfigNodePlan, error) {
	infos, err := librke.GetDockerInfo(node)
	if err != nil {
		return nil, err
	}

	hostAddress := node.Status.NodeConfig.Address
	hostDockerInfo := infos[hostAddress]

	appliedSpec, err := assemblers.AssembleRKEConfigSpec(cluster, *cluster.Status.AppliedSpec.DeepCopy(), uh.secretLister)
	if err != nil {
		return nil, err
	}

	rkeConfig := appliedSpec.RancherKubernetesEngineConfig.DeepCopy()
	nodeserver.FilterHostForSpec(rkeConfig, node)

	logrus.Debugf("[workerplan] The number of nodes sent to the plan: %v", len(rkeConfig.Nodes))
	svcOptions, err := uh.getServiceOptions(rkeConfig.Version, hostDockerInfo.OSType)
	if err != nil {
		return nil, err
	}

	plan, err := librke.New().GeneratePlan(uh.ctx, rkeConfig, infos, svcOptions)
	if err != nil {
		return nil, err
	}

	logrus.Debugf("[workerplan] getDockerInfo for node [%s] dockerInfo [%s]", node.Name, hostDockerInfo.DockerRootDir)

	np := &rketypes.RKEConfigNodePlan{}

	token, err := uh.systemAccountManager.GetOrCreateSystemClusterToken(cluster.Name)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create or get cluster token for share-mnt")
	}
	for _, tempNode := range plan.Nodes {
		if tempNode.Address == hostAddress {
			if hostDockerInfo.OSType == "windows" { // compatible with Windows
				np.Processes = nodeserver.EnhanceWindowsProcesses(tempNode.Processes)
			} else {
				np.Processes, err = nodeserver.AugmentProcesses(token, tempNode.Processes, true,
					node.Status.NodeConfig.HostnameOverride, cluster, uh.secretLister)
				if err != nil {
					return np, err
				}
			}
			if cluster.Annotations[clusterprovisioner.RkeRestoreAnnotation] == "true" {
				// This is for the node agent to be able to detect changes in the node plan for kubelet,
				// and therefore to recreate the kubelet container on worker nodes after RKE restores an etcd snapshot.
				// The value is not meant to represent the node version, it is chosen because it increases linearly.
				newEnvVar := fmt.Sprintf("%s=%d", restoreKey, cluster.Status.NodeVersion)
				logrus.Debugf("[workerplan] adding/updating env var [%s] on node [%s]", newEnvVar, node.Name)
				np.Processes[kubeletProcess] = AddEnvVarToProcess(np.Processes[kubeletProcess], newEnvVar)
			} else {
				// A normal, non-restoring-etcd-snapshot, updating on the cluster drops the above env var from the new plan,
				// which results in a change in the plan that, in theory, should trigger the redeployment of the kubelet container.
				// However, currently it does not happen which is a bug (GH-43308).
				// Adding the env var back is to avoid regression in the future when the bug is fixed.
				if node.Status.NodePlan != nil && node.Status.NodePlan.Plan != nil {
					oldProcess, found := node.Status.NodePlan.Plan.Processes[kubeletProcess]
					if found {
						for _, env := range oldProcess.Env {
							if strings.HasPrefix(env, restoreKey) {
								np.Processes[kubeletProcess] = AddEnvVarToProcess(np.Processes[kubeletProcess], env)
								break
							}
						}
					}
				}
			}

			np.Processes = nodeserver.AppendTaintsToKubeletArgs(np.Processes, node.Status.NodeConfig.Taints)
			np.Files = tempNode.Files

			return np, nil
		}
	}

	return nil, fmt.Errorf("[workerplan] failed to find plan for %s", hostAddress)

}

func (uh *upgradeHandler) getServiceOptions(k8sVersion string, osType string) (map[string]interface{}, error) {
	data := map[string]interface{}{}
	svcOptions, err := kd.GetRKEK8sServiceOptions(k8sVersion, uh.serviceOptionsLister,
		uh.serviceOptions, uh.sysImagesLister, uh.sysImages, kd.Linux)
	if err != nil {
		logrus.Errorf("getK8sServiceOptions: k8sVersion %s [%v]", k8sVersion, err)
		return data, err
	}
	if svcOptions != nil {
		data["k8s-service-options"] = svcOptions
	}
	if osType == "windows" {
		svcOptionsWindows, err := kd.GetRKEK8sServiceOptions(k8sVersion, uh.serviceOptionsLister,
			uh.serviceOptions, uh.sysImagesLister, uh.sysImages, kd.Windows)
		if err != nil {
			logrus.Errorf("getK8sServiceOptionsWindows: k8sVersion %s [%v]", k8sVersion, err)
			return data, err
		}
		if svcOptionsWindows != nil {
			data["k8s-windows-service-options"] = svcOptionsWindows
		}
	}
	return data, nil
}

func planChangedForUpgrade(newPlan *rketypes.RKEConfigNodePlan, oldPlan *rketypes.RKEConfigNodePlan) bool {
	if newPlan == nil || oldPlan == nil {
		return true
	}
	newProcesses := newPlan.Processes
	oldProcesses := oldPlan.Processes

	if len(newProcesses) != len(oldProcesses) {
		logrus.Infof("number of processes changed: old: %v new: %v", len(oldProcesses), len(newProcesses))
		return true
	}

	for k, newProcess := range newProcesses {
		if strings.Contains(k, "share-mnt") {
			// don't need to upgrade if share-mnt changed
			continue
		}
		oldProcess, ok := oldProcesses[k]
		if !ok {
			return true
		}

		if processChanged(newProcess, oldProcess) {
			return true
		}
	}

	return false
}

func processChanged(newp rketypes.Process, oldp rketypes.Process) bool {
	name := newp.Name

	if oldp.Image != newp.Image {
		logrus.Infof("image changed for [%s] old: %s new: %s", name, oldp.Image, newp.Image)
		return true
	}

	if sliceChangedUnordered(oldp.Command, newp.Command) {
		logrus.Infof("command changed for [%s] old: %v new: %v", name, oldp.Command, newp.Command)
		return true
	}

	if sliceChangedUnordered(oldp.Env, newp.Env) {
		logrus.Infof("env changed for [%s] old: %v new %v", name, oldp.Env, newp.Env)
		return true
	}

	if sliceChangedUnordered(oldp.Args, newp.Args) {
		logrus.Infof("args changed for [%s] old: %v new %v", name, oldp.Args, newp.Args)
		return true
	}

	if sliceChanged(oldp.Binds, newp.Binds) {
		logrus.Infof("binds changed for [%s] old: %v new %v", name, oldp.Binds, newp.Binds)
		return true
	}

	if sliceChanged(oldp.VolumesFrom, newp.VolumesFrom) {
		logrus.Infof("volumesFrom changed for [%s] old: %v new %v", name, oldp.VolumesFrom, newp.VolumesFrom)
		return true
	}

	if sliceChanged(oldp.Publish, newp.Publish) {
		logrus.Infof("publish changed for [%s] old: %v new %v", name, oldp.Publish, newp.Publish)
		return true
	}

	oldProcess := forCompareProcess(oldp)
	newProcess := forCompareProcess(newp)

	if !reflect.DeepEqual(oldProcess, newProcess) {
		logrus.Infof("process changed for [%s] old: %#v new %#v", name, oldProcess, newProcess)
		return true
	}

	return false

}

type compareProcess struct {
	Labels      map[string]string
	NetworkMode string
	PidMode     string
	Privileged  bool
}

func forCompareProcess(p rketypes.Process) compareProcess {
	return compareProcess{
		Labels:      p.Labels,
		NetworkMode: p.NetworkMode,
		PidMode:     p.PidMode,
		Privileged:  p.Privileged,
	}
}

func sliceChangedUnordered(olds, news []string) bool {
	oldMap := sliceToMap(olds)
	newMap := sliceToMap(news)

	if !reflect.DeepEqual(oldMap, newMap) {
		return true
	}
	return false
}

func sliceChanged(olds, news []string) bool {
	if len(olds) == 0 && len(news) == 0 {
		// DeepEqual considers []string{} and []string(nil) as different
		return false
	}

	if !reflect.DeepEqual(olds, news) {
		return true
	}

	return false
}

func planChangedForUpdate(newPlan, oldPlan *rketypes.RKEConfigNodePlan) bool {
	// files passed in node config
	if !reflect.DeepEqual(newPlan.Files, oldPlan.Files) {
		return true
	}
	// things that aren't included to restart container
	for k, newProcess := range newPlan.Processes {
		if oldProcess, ok := oldPlan.Processes[k]; ok {
			if oldProcess.Name != newProcess.Name {
				return true
			}

			if oldProcess.HealthCheck.URL != newProcess.HealthCheck.URL {
				return true
			}

			if oldProcess.RestartPolicy != newProcess.RestartPolicy {
				return true
			}

			if oldProcess.ImageRegistryAuthConfig != newProcess.ImageRegistryAuthConfig {
				return true
			}

			if strings.Contains(k, "share-mnt") {
				// don't need to upgrade if share-mnt changed
				if processChanged(newProcess, oldProcess) {
					return true
				}
			}
		}
	}

	return false

}

func sliceToMap(s []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range s {
		m[v] = true
	}
	return m
}

// AddEnvVarToProcess adds or overwrites the existing env var in target process
func AddEnvVarToProcess(process rketypes.Process, newEnvVar string) rketypes.Process {
	if process.Name == "" || process.Image == "" || newEnvVar == "" {
		return process
	}
	targetKey := strings.SplitN(newEnvVar, "=", 2)[0]
	found := false
	for i, env := range process.Env {
		k := strings.SplitN(env, "=", 2)[0]
		if k == targetKey {
			process.Env[i] = newEnvVar
			found = true
			break
		}
	}
	if !found {
		process.Env = append(process.Env, newEnvVar)
	}
	return process
}
