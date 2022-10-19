package kubeadm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
	v13 "k8s.io/api/core/v1"
	v14 "k8s.io/api/rbac/v1"
	apimachineryErrors "k8s.io/apimachinery/pkg/api/errors"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/kubeclipper/kubeclipper/cmd/kcctl/app/options"
	agentconfig "github.com/kubeclipper/kubeclipper/pkg/agent/config"
	"github.com/kubeclipper/kubeclipper/pkg/cli/config"
	"github.com/kubeclipper/kubeclipper/pkg/clustermanage"
	"github.com/kubeclipper/kubeclipper/pkg/constatns"
	"github.com/kubeclipper/kubeclipper/pkg/logger"
	"github.com/kubeclipper/kubeclipper/pkg/query"
	"github.com/kubeclipper/kubeclipper/pkg/scheme/common"
	v1 "github.com/kubeclipper/kubeclipper/pkg/scheme/core/v1"
	"github.com/kubeclipper/kubeclipper/pkg/utils/sshutils"
)

func init() {
	clustermanage.RegisterProvider(&Kubeadm{})
}

const ProviderKubeadm = "kubeadm"

type Kubeadm struct {
	Operator clustermanage.Operator
	Provider v1.CloudProvider
	Config   Config
}

func (r Kubeadm) ClusterType() string {
	return ProviderKubeadm
}

func (r Kubeadm) InitCloudProvider(operator clustermanage.Operator, provider v1.CloudProvider) (clustermanage.CloudProvider, error) {
	return NewKubeadm(operator, provider)
}

func (r Kubeadm) GetKubeConfig(ctx context.Context, clusterName string) (string, error) {
	// get it by kc's own kubeadm method, without any processing here
	return "", nil
}

func (r Kubeadm) GetCertification(ctx context.Context, clusterName string) ([]v1.Certification, error) {
	// get it by kc's own kubeadm method, without any processing here
	return nil, nil
}

type Config struct {
	// APIEndpoint kubeadm apiServer address
	APIEndpoint string `json:"apiEndpoint,omitempty"`
	KubeConfig  string `json:"kubeConfig"`
	ClusterName string `json:"clusterName"`
}

func NewKubeadm(operator clustermanage.Operator, provider v1.CloudProvider) (clustermanage.CloudProvider, error) {
	conf, err := rawToConfig(provider.Config)
	if err != nil {
		return nil, err
	}
	r := Kubeadm{
		Operator: operator,
		Provider: provider,
		Config:   conf,
	}
	return &r, nil
}

func (r Kubeadm) ToWrapper() (Wrapper, error) {
	var err error
	w := Wrapper{}
	w.KubeCli, err = NewKubeClient(r.Provider.Config)
	if err != nil {
		return w, err
	}
	w.ProviderName = r.Provider.Name
	w.APIEndpoint = r.Config.APIEndpoint
	w.KubeConfig = r.Config.KubeConfig
	w.Region = r.Provider.Region
	w.ClusterName = r.Config.ClusterName

	return w, nil
}

// Sync keep cluster consistent in kc and kubeadm.
/*
1. client-go connect kube-apiServer
2. get cluster info
3. create or update kc cluster
*/
func (r Kubeadm) Sync(ctx context.Context) error {
	log := logger.FromContext(ctx)
	log.Debugf("beginning sync provider %s", r.Provider.Name)

	w, err := r.ToWrapper()
	if err != nil {
		return err
	}
	// 1.get kubeadm cluster info
	clu, err := w.ClusterInfo()
	if err != nil {
		return err
	}

	err = r.importClusterToKC(ctx, clu)
	if err != nil {
		return err
	}

	err = r.clusterServiceAccount(ctx, v1.ActionInstall)
	if err != nil {
		return err
	}

	log.Debugf("sync provider %s successfully", r.Provider.Name)

	return nil
}

// Cleanup clean provider's all cluster & node in kc.
/*
1.list kc clusters
2.drain cluster's node
3.delete cluster
*/
func (r Kubeadm) Cleanup(ctx context.Context) error {
	log := logger.FromContext(ctx)
	log.Debugf("beginning cleanup provider %s", r.Provider.Name)

	// 2. drain nodes first
	clu, err := r.Operator.ClusterLister.Get(r.Config.ClusterName)
	if err != nil {
		if apimachineryErrors.IsNotFound(err) {
			logger.Debugf("get cluster result: %v", err)
			return nil
		}
		return errors.WithMessage(err, "get cluster failed")
	}
	nodes, err := r.listKCNode(clu.Name)
	if err != nil {
		return errors.WithMessagef(err, "load cluster %s's node", clu.Name)
	}
	log.Debugf("[cleanup] drain cluster %s's node count:%#v", clu.Name, len(nodes))

	for _, node := range nodes {
		log.Debugf("[cleanup] drain cluster %s's nodes:%v", clu.Name, node.Name)
		// origin node use deployConfig.ssh,others use provider.ssh
		if _, isOriginNode := node.Annotations[common.AnnotationOriginNode]; isOriginNode {
			delete(node.Labels, common.LabelNodeRole)
			_, err = r.Operator.NodeWriter.UpdateNode(ctx, node)
			if err != nil {
				log.Errorf("origin node(%s) update failed", node.Name)
				return err
			}
			continue
		}
		if err = r.drainAgent(node.Status.Ipv4DefaultIP, node.Name, r.ssh()); err != nil {
			return errors.WithMessagef(err, "drain cluster %s's node %s", clu.Name, node.Name)
		}
	}

	err = r.clusterServiceAccount(ctx, v1.ActionUninstall)
	if err != nil {
		return err
	}

	// 3. delete cluster
	// NOTE: must delete cluster after drain node
	// because cluster controller will remove node's label,if delete cluster first
	// then,the note will lost connection about this rancher cluster,case we can't drain it.
	if err = r.Operator.ClusterWriter.DeleteCluster(ctx, clu.Name); err != nil {
		return errors.WithMessagef(err, "delete cluster  %s", clu.Name)
	}

	log.Debugf("cleanup provider %s successfully", r.Provider.Name)
	return nil
}

func rawToConfig(config runtime.RawExtension) (Config, error) {
	var conf Config
	data, err := config.MarshalJSON()
	if err != nil {
		return conf, err
	}
	if err = json.Unmarshal(data, &conf); err != nil {
		return conf, err
	}
	return conf, nil
}

func (r Kubeadm) importClusterToKC(ctx context.Context, clu *v1.Cluster) error {
	log := logger.FromContext(ctx)
	log.Debugf("beginning import provider %s's cluster [%s] to kc", r.Provider.Name, clu.Name)

	// sync cluster's node first
	err := r.syncNode(ctx, clu)
	if err != nil {
		return errors.WithMessagef(err, "sync cluster %s's node", clu.Name)
	}
	// then,import cluster
	oldClu, err := r.Operator.ClusterLister.Get(clu.Name)
	if err != nil {
		// create,if not exists
		if apimachineryErrors.IsNotFound(err) {
			if _, err = r.Operator.ClusterWriter.CreateCluster(context.TODO(), clu); err != nil {
				return errors.WithMessagef(err, "create cluster %s", clu.Name)
			}
		} else {
			return errors.WithMessagef(err, "check cluster %s exits", clu.Name)
		}
	} else {
		// update,if exists
		// get resourceVersion for update
		clu.ObjectMeta.ResourceVersion = oldClu.ObjectMeta.ResourceVersion
		clu.Annotations[common.AnnotationDescription] = oldClu.Annotations[common.AnnotationDescription]
		_, err = r.Operator.ClusterWriter.UpdateCluster(context.TODO(), clu)
		if err != nil {
			return errors.WithMessagef(err, "update cluster %s", clu.Name)
		}
	}

	log.Debugf("import provider %s's cluster [%v] successfully", r.Provider.Name, clu.Name)
	return nil
}

func (r Kubeadm) ssh() *sshutils.SSH {
	ssh := &sshutils.SSH{
		User:              r.Provider.SSH.User,
		Port:              r.Provider.SSH.Port,
		ConnectionTimeout: nil,
	}
	if r.Provider.SSH.PrivateKey != "" {
		decodeString, _ := base64.StdEncoding.DecodeString(r.Provider.SSH.PrivateKey)
		ssh.PrivateKey = string(decodeString)
	}

	if r.Provider.SSH.Password != "" {
		decodeString, _ := base64.StdEncoding.DecodeString(r.Provider.SSH.Password)
		ssh.Password = string(decodeString)
	}

	if r.Provider.SSH.PrivateKeyPassword != "" {
		decodeString, _ := base64.StdEncoding.DecodeString(r.Provider.SSH.PrivateKeyPassword)
		ssh.PkPassword = string(decodeString)
	}

	return ssh
}

func (r Kubeadm) listKCNode(clusterName string) ([]*v1.Node, error) {
	requirement, err := labels.NewRequirement(common.LabelClusterName, selection.Equals, []string{clusterName})
	if err != nil {
		return nil, err
	}
	return r.Operator.NodeLister.List(labels.NewSelector().Add(*requirement))
}

// syncNode keep cluster's node consistent in kc and kubeadm.
/*
1.list cluster's nodes
2.sync node
	if not exists, means it's a new node,we need deploy kc-agent to it.
	if node already exists,do nothing,but if add origin to kubeadm cluster,will match this case,we need check node's label.
3.delete legacy node from kc: in kc but not in kubeadm,it's a legacy node,we need delete it.
	not origin node,drain it
	origin node,just clean label&annotations to mark node free
*/
func (r Kubeadm) syncNode(ctx context.Context, clu *v1.Cluster) error {
	// first import
	addNodes, delNodes, err := r.NodeDiff(clu)
	if err != nil {
		return err
	}

	for _, no := range addNodes {
		err = r.deployKCAgent(ctx, no, clu.Labels[common.LabelTopologyRegion])
		if err != nil {
			return errors.WithMessagef(err, "node(%s) deploy kc-agent in kc", no.ID)
		}
	}
	for _, no := range delNodes {
		if _, isOriginNode := no.Annotations[common.AnnotationOriginNode]; isOriginNode {
			// mark to free
			if err = r.markToFree(ctx, no); err != nil {
				return errors.WithMessagef(err, "mark node to free")
			}
			logger.Infof("sync cluster %s's node,mark origin node %s to free,because not in rancher", clu.Name, no.Name)
			continue
		}
		err = r.drainAgent(no.Status.Ipv4DefaultIP, no.Name, r.ssh())
		if err != nil {
			return errors.WithMessagef(err, "node(%s) drain kc-agent in kc", no.Status.Ipv4DefaultIP)
		}
	}

	for i := range clu.Masters {
		r.replaceIDToIP(&clu.Masters[i])
	}
	for i := range clu.Workers {
		r.replaceIDToIP(&clu.Workers[i])
	}

	logger.Debugf("sync cluster %s's node successfully", clu.Name)
	return nil
}

func (r Kubeadm) markToFree(ctx context.Context, node *v1.Node) error {
	delete(node.Labels, common.LabelNodeRole)
	delete(node.Labels, common.LabelClusterName)
	delete(node.Annotations, common.AnnotationProviderNodeID)
	delete(node.Annotations, common.AnnotationOriginNode)
	_, err := r.Operator.NodeWriter.UpdateNode(ctx, node)
	return err
}

func (r Kubeadm) deployKCAgent(ctx context.Context, node *v1.WorkerNode, region string) error {
	ip := node.ID
	node.ID = uuid.New().String()
	log := logger.FromContext(ctx)
	log.Debugf("beginning deploy kc agent to node agent:%s ip:%s", node.ID, ip)

	// 1.download kc-agent binary from kc-server & get certs from configmap.
	deployConfig, err := r.getDeployConfig()
	if err != nil {
		return errors.WithMessage(err, "getDeployConfig")
	}

	originalID, originalRegion, active := r.agentStatus(ip)
	if originalID != "" {
		node.ID = originalID
	}
	no, nodeErr := r.Operator.NodeLister.Get(originalID)
	if nodeErr != nil && !apimachineryErrors.IsNotFound(nodeErr) {
		return nodeErr
	}
	if active && no != nil && no.Labels[common.LabelTopologyRegion] == originalRegion {
		if !deployConfig.Agents.Exists(ip) {
			meta := options.Metadata{
				Region: region,
			}
			err = r.updateDeployConfigAgents(ip, &meta, "add")
			if err != nil {
				logger.Errorf("add agent ip to deploy config failed: %v", err)
				return err
			}
		}
		logger.Warnf("update deploy-config agent failed: %v", err)
		return nil
	}
	// download http://192.168.10.123:8081/kc/kubeclipper-agent
	url := fmt.Sprintf("http://%s:%v/kc/kubeclipper-agent", deployConfig.ServerIPs[0], deployConfig.StaticServerPort)
	cmdList := []string{
		"systemctl stop kc-agent || true",
		fmt.Sprintf("curl %s -o /usr/local/bin/kubeclipper-agent", url),
		"chmod +x /usr/local/bin/kubeclipper-agent",
	}

	for _, cmd := range cmdList {
		ret, err := sshutils.SSHCmdWithSudo(r.ssh(), ip, cmd)
		if err != nil {
			return errors.WithMessagef(err, "run cmd [%s] on node [%s]", cmd, ip)
		}
		if err = ret.Error(); err != nil {
			return errors.WithMessage(err, ret.String())
		}
	}

	ca, cliCert, cliKey, err := r.gerCerts()
	if err != nil {
		return errors.WithMessage(err, "gerCerts from kc configmap")
	}
	destCa := filepath.Join(options.DefaultKcAgentConfigPath, options.DefaultCaPath, "ca.crt")
	destCert := filepath.Join(options.DefaultKcAgentConfigPath, options.DefaultNatsPKIPath, "kc-server-nats-client.crt")
	destKey := filepath.Join(options.DefaultKcAgentConfigPath, options.DefaultNatsPKIPath, "kc-server-nats-client.key")
	cmds := []string{
		"mkdir -p /etc/kubeclipper-agent/pki/nats",
		sshutils.WrapEcho(string(ca), destCa),
		sshutils.WrapEcho(string(cliCert), destCert),
		sshutils.WrapEcho(string(cliKey), destKey),
	}

	for _, cmd := range cmds {
		ret, err := sshutils.SSHCmdWithSudo(r.ssh(), ip, cmd)
		if err != nil {
			return errors.WithMessagef(err, "run %s cmd", cmd)
		}
		if err = ret.Error(); err != nil {
			return errors.WithMessage(err, ret.String())
		}
	}

	// 2. generate kubeclipper-agent.yaml、systemd conf,then start kc-agent
	agentConfig, err := deployConfig.GetKcAgentConfigTemplateContent(options.Metadata{Region: region}, node.ID)
	if err != nil {
		return errors.WithMessage(err, "GetKcAgentConfigTemplateContent")
	}
	cmdList = []string{
		sshutils.WrapEcho(config.KcAgentService, "/usr/lib/systemd/system/kc-agent.service"),
		"mkdir -pv /etc/kubeclipper-agent",
		sshutils.WrapEcho(agentConfig, "/etc/kubeclipper-agent/kubeclipper-agent.yaml"),
		"systemctl daemon-reload && systemctl enable kc-agent && systemctl restart kc-agent",
	}
	for _, cmd := range cmdList {
		ret, err := sshutils.SSHCmdWithSudo(r.ssh(), ip, cmd)
		if err != nil {
			return errors.WithMessagef(err, "run %s cmd", cmd)
		}
		if err = ret.Error(); err != nil {
			return errors.WithMessage(err, ret.String())
		}
	}

	log.Debugf("deploy kc agent to node agent:%s ip:%s successfully", node.ID, ip)

	return nil
}

// drainAgent remote kc-agent for node,and delete node from kc-server
func (r Kubeadm) drainAgent(nodeIP, agentID string, ssh *sshutils.SSH) error {
	// 1. remove agent
	cmdList := []string{
		"systemctl disable kc-agent --now || true", // 	// disable agent service
		"rm -rf /usr/local/bin/kubeclipper-agent /etc/kubeclipper-agent /usr/lib/systemd/system/kc-agent.service ", // remove agent files
	}

	for _, cmd := range cmdList {
		ret, err := sshutils.SSHCmdWithSudo(ssh, nodeIP, cmd)
		if err != nil {
			return errors.WithMessagef(err, "run cmd %s on %s failed", cmd, nodeIP)
		}
		if err = ret.Error(); err != nil {
			return errors.WithMessage(err, ret.String())
		}
	}

	// 2. delete from etcd
	err := r.Operator.NodeWriter.DeleteNode(context.TODO(), agentID)
	if err != nil {
		return errors.WithMessagef(err, "delete node %s failed", agentID)
	}

	// 1.download kc-agent binary from kc-server & get certs from configmap.
	deployConfig, err := r.getDeployConfig()
	if err != nil {
		return errors.WithMessage(err, "getDeployConfig")
	}

	if !deployConfig.Agents.Exists(nodeIP) {
		err = r.updateDeployConfigAgents(nodeIP, nil, "del")
		if err != nil {
			logger.Errorf("add agent ip to deploy config failed: %v", err)
			return err
		}
	}

	return nil
}

func (r Kubeadm) gerCerts() (ca, natsCliCert, natsCliKey []byte, err error) {
	kcca, err := r.Operator.ConfigmapLister.Get("kc-ca")
	if err != nil {
		return nil, nil, nil, err
	}
	nats, err := r.Operator.ConfigmapLister.Get("kc-nats")
	if err != nil {
		return nil, nil, nil, err
	}

	ca, err = base64.StdEncoding.DecodeString(kcca.Data["ca.crt"])
	if err != nil {
		return nil, nil, nil, err
	}
	natsCliCert, err = base64.StdEncoding.DecodeString(nats.Data["kc-server-nats-client.crt"])
	if err != nil {
		return nil, nil, nil, err
	}
	natsCliKey, err = base64.StdEncoding.DecodeString(nats.Data["kc-server-nats-client.key"])
	if err != nil {
		return nil, nil, nil, err
	}

	return ca, natsCliCert, natsCliKey, nil
}

func (r Kubeadm) getDeployConfig() (*options.DeployConfig, error) {
	configMap, err := r.Operator.ConfigmapLister.Get("deploy-config")
	if err != nil {
		return nil, err
	}

	var c options.DeployConfig
	err = yaml.Unmarshal([]byte(configMap.Data["DeployConfig"]), &c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r Kubeadm) PreCheck(ctx context.Context) (bool, error) {
	providers, err := r.Operator.CloudProviderReader.ListCloudProviders(ctx, &query.Query{})
	if err != nil {
		return false, err
	}
	for _, prov := range providers.Items {
		if prov.Type != ProviderKubeadm {
			continue
		}
		if strings.Contains(prov.Config.String(), r.Config.ClusterName) {
			return false, fmt.Errorf("cluster name %s already exists", r.Config.ClusterName)
		}
		if strings.Contains(prov.Config.String(), r.Config.APIEndpoint) &&
			strings.Contains(prov.Config.String(), r.Config.KubeConfig) {
			return false, fmt.Errorf("cluster %s already exists", r.Config.ClusterName)
		}
	}
	cli, err := NewKubeClient(r.Provider.Config)
	if err != nil {
		return false, err
	}
	_, err = cli.ServerVersion()
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r Kubeadm) NodeDiff(clu *v1.Cluster) (addNodes []*v1.WorkerNode, delNodes []*v1.Node, err error) {
	oldNodes, err := r.listKCNode(clu.Name)
	if err != nil {
		return nil, nil, errors.WithMessagef(err, "list cluster %s's node in kc", clu.Name)
	}
	newNodes := make([]*v1.WorkerNode, 0)
	for i := range clu.Masters {
		newNodes = append(newNodes, &clu.Masters[i])
	}
	for i := range clu.Workers {
		newNodes = append(newNodes, &clu.Workers[i])
	}

	newNodeMap := make(map[string]*v1.WorkerNode)
	oldNodeMap := make(map[string]*v1.Node)
	delNodeMap := make(map[string]struct{})

	for i := range newNodes {
		newNodeMap[newNodes[i].ID] = newNodes[i]
	}
	for i := range oldNodes {
		oldNodeMap[oldNodes[i].Status.Ipv4DefaultIP] = oldNodes[i]
	}

	for k, v := range newNodeMap {
		if _, ok := oldNodeMap[k]; !ok {
			addNodes = append(addNodes, v)
		}
	}

	for k, v := range oldNodeMap {
		if _, ok := newNodeMap[k]; !ok {
			delNodes = append(delNodes, v)
			delNodeMap[v.Status.Ipv4DefaultIP] = struct{}{}
		}
	}

	return
}

func (r Kubeadm) agentStatus(ip string) (id, region string, active bool) {
	// check if kc-agent is running
	ret, err := sshutils.SSHCmdWithSudo(r.ssh(), ip,
		"systemctl --all --type service | grep kc-agent | grep running | wc -l")
	if err != nil {
		logger.Warnf("check node %s failed: %s", ip, err.Error())
		return "", "", false
	}
	if ret.StdoutToString("") == "0" {
		logger.Debugf("kc-agent service not exist on %s", ip)
		return "", "", false
	}

	ret, err = sshutils.SSHCmdWithSudo(r.ssh(), ip,
		"cat /etc/kubeclipper-agent/kubeclipper-agent.yaml")
	if err != nil {
		logger.Warnf("check node %s failed: %s", ip, err.Error())
		return "", "", true
	}

	agentConf := &agentconfig.Config{}
	err = yaml.Unmarshal([]byte(ret.Stdout), agentConf)
	if err != nil {
		logger.Warnf("node(%s) agent agentConf unmarshal failed: %s", ip, err.Error())
		return "", "", true
	}

	return agentConf.AgentID, agentConf.Metadata.Region, true
}

func (r Kubeadm) updateDeployConfigAgents(ip string, meta *options.Metadata, action string) error {
	deploy, err := r.Operator.ConfigmapLister.Get(constatns.DeployConfigConfigMapName)
	if err != nil {
		return fmt.Errorf("get deploy config failed: %v", err)
	}
	confString := deploy.Data[constatns.DeployConfigConfigMapKey]
	deployConfig := &options.DeployConfig{}
	err = yaml.Unmarshal([]byte(confString), deployConfig)
	if err != nil {
		return fmt.Errorf("deploy-config unmarshal failed: %v", err)
	}

	switch action {
	case "add":
		deployConfig.Agents.Add(ip, *meta)
	case "del":
		deployConfig.Agents.Delete(ip)
	}

	dcData, err := yaml.Marshal(deployConfig)
	if err != nil {
		return fmt.Errorf("deploy config marshal failed: %v", err)
	}
	deploy.Data[constatns.DeployConfigConfigMapKey] = string(dcData)
	_, err = r.Operator.ConfigmapWriter.UpdateConfigMap(context.TODO(), deploy)
	return err
}

func (r Kubeadm) replaceIDToIP(no *v1.WorkerNode) {
	address := net.ParseIP(no.ID)
	if address != nil {
		remoteID, _, _ := r.agentStatus(no.ID)
		if remoteID != "" {
			no.ID = remoteID
		}
	}
}

func (r Kubeadm) clusterServiceAccount(ctx context.Context, action v1.StepAction) error {
	w, err := r.ToWrapper()
	if err != nil {
		return err
	}
	sa := &v13.ServiceAccount{}
	sa.Name = "kc-server"

	crb := &v14.ClusterRoleBinding{}
	crb.Name = "kc-server"
	crb.RoleRef.Kind = "ClusterRole"
	crb.RoleRef.Name = "cluster-admin"
	crb.Subjects = []v14.Subject{{Kind: "ServiceAccount", Name: "kc-server", Namespace: "kube-system"}}

	switch action {
	case v1.ActionInstall:
		_, err = w.KubeCli.CoreV1().ServiceAccounts("kube-system").Create(ctx, sa, v12.CreateOptions{})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return err
		}

		_, err = w.KubeCli.RbacV1().ClusterRoleBindings().Create(ctx, crb, v12.CreateOptions{})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return err
		}
	case v1.ActionUninstall:
		err = w.KubeCli.CoreV1().ServiceAccounts("kube-system").Delete(ctx, sa.Name, v12.DeleteOptions{})
		if err != nil && !strings.Contains(err.Error(), "not found") {
			return err
		}

		err = w.KubeCli.RbacV1().ClusterRoleBindings().Delete(ctx, crb.Name, v12.DeleteOptions{})
		if err != nil && !strings.Contains(err.Error(), "not found") {
			return err
		}
	}

	return nil
}