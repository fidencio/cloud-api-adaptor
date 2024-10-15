// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package provisioner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

var trusteeRepoPath string

func getHardwarePlatform() (string, error) {
	out, err := exec.Command("uname", "-m").Output()
	return strings.TrimSuffix(string(out), "\n"), err
}

func getOverlaysPath() (string, error) {
	platform, err := getHardwarePlatform()
	if err != nil {
		return "", err
	}

	overlaysPath := "overlays"
	if platform == "s390x" && os.Getenv("IBM_SE_CREDS_DIR") != "" {
		overlaysPath += "/ibm-se"
	}
	return overlaysPath, nil
}

func getKbsKubernetesFilePath() string {
	return filepath.Join(trusteeRepoPath, "/kbs/config/kubernetes/")
}

func NewKeyBrokerService(clusterName string, cfg *envconf.Config) (*KeyBrokerService, error) {
	e2e_dir, err := os.Getwd()
	if err != nil {
		err = fmt.Errorf("getting the current working directory: %w\n", err)
		log.Errorf("%v", err)
		return nil, err
	}
	trusteeRepoPath = filepath.Join(e2e_dir, "../trustee")

	log.Info("creating key.bin")

	// Create secret
	content := []byte("This is my cluster name: " + clusterName)
	overlaysPath, err := getOverlaysPath()
	if err != nil {
		return nil, err
	}

	filePath := filepath.Join(getKbsKubernetesFilePath(), overlaysPath, "key.bin")
	// Create the file.
	file, err := os.Create(filePath)
	if err != nil {
		err = fmt.Errorf("creating file: %w\n", err)
		log.Errorf("%v", err)
		return nil, err
	}
	defer file.Close()

	// Write the content to the file.
	err = saveToFile(filePath, content)
	if err != nil {
		err = fmt.Errorf("writing to the file: %w\n", err)
		log.Errorf("%v", err)
		return nil, err
	}

	kbsCert := filepath.Join(getKbsKubernetesFilePath(), "base/kbs.pem")
	if _, err := os.Stat(kbsCert); os.IsNotExist(err) {
		kbsKey := filepath.Join(getKbsKubernetesFilePath(), "base/kbs.key")
		keyOutputFile, err := os.Create(kbsKey)
		if err != nil {
			err = fmt.Errorf("creating key file: %w\n", err)
			log.Errorf("%v", err)
			return nil, err
		}
		defer keyOutputFile.Close()

		pubKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			err = fmt.Errorf("generating Ed25519 key pair: %w\n", err)
			log.Errorf("%v", err)
			return nil, err
		}

		b, err := x509.MarshalPKCS8PrivateKey(privateKey)
		if err != nil {
			err = fmt.Errorf("MarshalPKCS8PrivateKey private key: %w\n", err)
			log.Errorf("%v", err)
			return nil, err
		}

		privateKeyPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: b,
		})

		// Save private key to file
		err = saveToFile(kbsKey, privateKeyPEM)
		if err != nil {
			err = fmt.Errorf("saving private key to file: %w\n", err)
			log.Errorf("%v", err)
			return nil, err
		}

		b, err = x509.MarshalPKIXPublicKey(pubKey)
		if err != nil {
			err = fmt.Errorf("MarshalPKIXPublicKey Ed25519 public key: %w\n", err)
			log.Errorf("%v", err)
			return nil, err
		}

		publicKeyPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "PUBLIC KEY",
			Bytes: b,
		})

		// Save public key to file
		err = saveToFile(kbsCert, publicKeyPEM)
		if err != nil {
			err = fmt.Errorf("saving public key to file: %w\n", err)
			log.Errorf("%v", err)
			return nil, err
		}

	}

	customPCCSURL := os.Getenv("CUSTOM_PCCS_URL")
	if customPCCSURL != "" {
		log.Info("CUSTOM_PCCS_URL is provided, write custom PCCS config")
		configFilePath := filepath.Join(getKbsKubernetesFilePath(), "custom_pccs/sgx_default_qcnl.conf")
		collateralUrl := "https://api.trustedservices.intel.com/sgx/certification/v4/"
		config := fmt.Sprintf(`{ "pccs_url": "%s", "collateral_service": "%s"}`, customPCCSURL, collateralUrl)
		err = saveToFile(configFilePath, []byte(config))
		if err != nil {
			return nil, err
		}
	}

	// IBM_SE_CREDS_DIR describe at https://github.com/confidential-containers/trustee/blob/main/kbs/config/kubernetes/README.md#deploy-kbs
	ibmseCredsDir := os.Getenv("IBM_SE_CREDS_DIR")
	if ibmseCredsDir != "" {
		log.Info("IBM_SE_CREDS_DIR is providered, deploy KBS with IBM SE verifier")
		// We always deploy the KBS pod to first worker node
		workerNodeIP, workerNodeName, _ := getFirstWorkerNodeIPAndName(cfg)
		log.Infof("Copying IBM_SE_CREDS files to first worker node: %s", workerNodeIP)
		err := copyGivenFilesToWorkerNode(ibmseCredsDir, workerNodeIP)
		if err != nil {
			return nil, err
		}
		log.Infof("Creating PV for kbs with ibm-se")

		overlaysPath, err := getOverlaysPath()
		if err != nil {
			return nil, err
		}
		pvFilePath := filepath.Join(getKbsKubernetesFilePath(), overlaysPath, "pv.yaml")
		err = createPVonTargetWorkerNode(pvFilePath, workerNodeName, cfg)
		if err != nil {
			return nil, err
		}
		patchFile := filepath.Join(getKbsKubernetesFilePath(), overlaysPath, "patch.yaml")
		// skip the SE related certs check as we are running the test case on a dev machine
		err = skipSeCertsVerification(patchFile)
		if err != nil {
			return nil, err
		}
	}

	overlay, err := NewBaseKbsInstallOverlay(trusteeRepoPath)
	if err != nil {
		return nil, err
	}

	return &KeyBrokerService{
		installOverlay: overlay,
		endpoint:       "",
	}, nil
}

func saveToFile(filename string, content []byte) error {
	// Save contents to file
	err := os.WriteFile(filename, content, 0644)
	if err != nil {
		return fmt.Errorf("writing contents to file: %w", err)
	}
	return nil
}

func skipSeCertsVerification(patchFile string) error {
	data, err := os.ReadFile(patchFile)
	if err != nil {
		return fmt.Errorf("failed to read file: %v", err)
	}
	content := string(data)
	content = strings.Replace(content, "false", "true", -1)
	err = os.WriteFile(patchFile, []byte(content), 0644)
	if err != nil {
		return fmt.Errorf("failed to write file: %v", err)
	}
	return nil
}

func createPVonTargetWorkerNode(pvFilePath, nodeName string, cfg *envconf.Config) error {
	data, err := os.ReadFile(pvFilePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %v", err)
	}
	content := string(data)
	content = strings.Replace(content, "${IBM_SE_CREDS_DIR}", "/root/ibmse", -1)
	content = strings.Replace(content, "${NODE_NAME}", nodeName, -1)
	err = os.WriteFile(pvFilePath, []byte(content), 0644)
	if err != nil {
		return fmt.Errorf("failed to write file: %v", err)
	}

	cmd := exec.Command("kubectl", "apply", "-f", pvFilePath)
	cmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG="+cfg.KubeconfigFile()))
	stdoutStderr, err := cmd.CombinedOutput()
	log.Tracef("%v, output: %s", cmd, stdoutStderr)
	if err != nil {
		return err
	}

	return nil
}

func getFirstWorkerNodeIPAndName(cfg *envconf.Config) (string, string, error) {
	client, err := cfg.NewClient()
	if err != nil {
		return "", "", err
	}
	nodeList := &corev1.NodeList{}
	if err := client.Resources("").List(context.TODO(), nodeList); err != nil {
		return "", "", err
	}
	// Filter out control plane nodes and get the IP of the first worker node
	for _, node := range nodeList.Items {
		if isWorkerNode(&node) {
			return node.Status.Addresses[0].Address, node.Name, nil
		}
	}
	return "", "", fmt.Errorf("no worker nodes found")
}

func isWorkerNode(node *corev1.Node) bool {
	// Check for the existence of the label or taint that identifies control plane nodes
	_, isMaster := node.Labels["node-role.kubernetes.io/master"]
	_, isControlPlane := node.Labels["node-role.kubernetes.io/control-plane"]
	if isMaster || isControlPlane {
		return false
	}
	return true
}

func copyGivenFilesToWorkerNode(sourceDir, targetNodeIP string) error {
	// Step 1: Compress the source directory using tar
	tarFilePath, err := compressDirectory(sourceDir)
	if err != nil {
		return fmt.Errorf("failed to compress directory: %v", err)
	}
	defer os.Remove(tarFilePath) // Clean up the temporary tar file

	// Step 2: Transfer the compressed file to the target node using SCP
	targetFilePath := "/tmp/" + filepath.Base(tarFilePath)
	err = transferFile(tarFilePath, targetNodeIP, targetFilePath)
	if err != nil {
		return fmt.Errorf("failed to transfer file: %v", err)
	}

	// Step 3: Decompress the file on the target node
	err = decompressFileOnTargetNode(targetNodeIP, targetFilePath, "/root")
	if err != nil {
		return fmt.Errorf("failed to decompress file on target node: %v", err)
	}

	return nil
}

func compressDirectory(sourceDir string) (string, error) {
	tarFilePath := sourceDir + ".tar.gz"
	cmd := exec.Command("tar", "-czf", tarFilePath, "-C", filepath.Dir(sourceDir), filepath.Base(sourceDir))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return tarFilePath, nil
}

func transferFile(localFilePath, targetNodeIP, remoteFilePath string) error {
	cmd := exec.Command("scp", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", localFilePath, fmt.Sprintf("root@%s:%s", targetNodeIP, remoteFilePath))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func decompressFileOnTargetNode(targetNodeIP, remoteFilePath, targetDir string) error {
	cmd := exec.Command("ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", fmt.Sprintf("root@%s", targetNodeIP), fmt.Sprintf("tar -xzf %s -C %s", remoteFilePath, targetDir))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func NewBaseKbsInstallOverlay(installDir string) (InstallOverlay, error) {
	log.Info("Creating kbs install overlay")
	overlay, err := NewKustomizeOverlay(filepath.Join(installDir, "kbs/config/kubernetes/base/"))
	if err != nil {
		return nil, err
	}

	return &KbsInstallOverlay{
		overlay: overlay,
	}, nil
}

func NewKbsInstallOverlay(installDir string) (InstallOverlay, error) {
	log.Info("Creating kbs install overlay")
	platform, err := getHardwarePlatform()
	if err != nil {
		return nil, err
	}

	var overlayFolder string
	if platform == "x86_64" && os.Getenv("CUSTOM_PCCS_URL") != "" {
		log.Info("CUSTOM_PCCS_URL is provided on x86_64, deploy with custom pccs config")
		overlayFolder = "kbs/config/kubernetes/custom_pccs"
	} else {
		overlayFolder = "kbs/config/kubernetes/nodeport/"
	}

	overlay, err := NewKustomizeOverlay(filepath.Join(installDir, overlayFolder))
	if err != nil {
		return nil, err
	}

	return &KbsInstallOverlay{
		overlay: overlay,
	}, nil
}

func (lio *KbsInstallOverlay) Apply(ctx context.Context, cfg *envconf.Config) error {
	return lio.overlay.Apply(ctx, cfg)
}

func (lio *KbsInstallOverlay) Delete(ctx context.Context, cfg *envconf.Config) error {
	return lio.overlay.Delete(ctx, cfg)
}

func (lio *KbsInstallOverlay) Edit(ctx context.Context, cfg *envconf.Config, props map[string]string) error {
	// No edits needed here at the moment
	return nil
}

func getNodeIPForSvc(deploymentName string, service corev1.Service, cfg *envconf.Config) (string, error) {
	client, err := cfg.NewClient()
	if err != nil {
		return "", err
	}
	podList := &corev1.PodList{}
	if err := client.Resources(service.Namespace).List(context.TODO(), podList); err != nil {
		return "", err
	}

	nodeList := &corev1.NodeList{}
	if err := client.Resources("").List(context.TODO(), nodeList); err != nil {
		return "", err
	}

	var matchingPod *corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Labels["app"] == deploymentName {
			matchingPod = pod
			break
		}
	}

	for _, node := range nodeList.Items {
		if node.Name == matchingPod.Spec.NodeName {
			return node.Status.Addresses[0].Address, nil
		}
	}

	return "", fmt.Errorf("Node IP not found for Service %s", service.Name)
}

func (p *KeyBrokerService) GetCachedKbsEndpoint() (string, error) {
	if p.endpoint != "" {
		return p.endpoint, nil
	}
	return "", fmt.Errorf("KeyBrokerService not found")
}

func (p *KeyBrokerService) GetKbsEndpoint(ctx context.Context, cfg *envconf.Config) (string, error) {
	client, err := cfg.NewClient()
	if err != nil {
		return "", err
	}

	namespace := "coco-tenant"
	serviceName := "kbs"
	deploymentName := "kbs"

	resources := client.Resources(namespace)

	kbsDeployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: namespace}}
	fmt.Printf("Wait for the %s deployment be available\n", deploymentName)
	if err = wait.For(conditions.New(resources).DeploymentConditionMatch(kbsDeployment, appsv1.DeploymentAvailable, corev1.ConditionTrue),
		wait.WithTimeout(time.Minute*2)); err != nil {
		return "", err
	}

	services := &corev1.ServiceList{}
	if err := resources.List(context.TODO(), services); err != nil {
		return "", err
	}

	for _, service := range services.Items {
		if service.ObjectMeta.Name == serviceName {
			// Ensure the service is of type NodePort
			if service.Spec.Type != corev1.ServiceTypeNodePort {
				return "", fmt.Errorf("Service %s is not of type NodePort", "kbs")
			}

			var nodePort int32
			// Extract NodePort
			if len(service.Spec.Ports) > 0 {
				nodePort = service.Spec.Ports[0].NodePort
			} else {
				return "", fmt.Errorf("NodePort is not configured for Service %s", "kbs")
			}

			nodeIP, err := getNodeIPForSvc(deploymentName, service, cfg)
			if err != nil {
				return "", err
			}

			p.endpoint = fmt.Sprintf("http://%s:%d", nodeIP, nodePort)
			return p.endpoint, nil
		}
	}

	return "", fmt.Errorf("Service %s not found", serviceName)
}

func (p *KeyBrokerService) EnableKbsCustomizedResourcePolicy(customizedOpaFile string) error {
	kbsClientDir := filepath.Join(trusteeRepoPath, "target/release")
	privateKey := filepath.Join(getKbsKubernetesFilePath(), "base/kbs.key")
	policyFile := filepath.Join(trusteeRepoPath, "kbs/sample_policies", customizedOpaFile)
	log.Info("EnableKbsCustomizedPolicy: ", policyFile)
	cmd := exec.Command("./kbs-client", "--url", p.endpoint, "config", "--auth-private-key", privateKey, "set-resource-policy", "--policy-file", policyFile)
	cmd.Dir = kbsClientDir
	cmd.Env = os.Environ()
	stdoutStderr, err := cmd.CombinedOutput()
	log.Tracef("%v, output: %s", cmd, stdoutStderr)
	if err != nil {
		return err
	}
	return nil
}

func (p *KeyBrokerService) EnableKbsCustomizedAttestationPolicy(customizedOpaFile string) error {
	kbsClientDir := filepath.Join(trusteeRepoPath, "target/release")
	privateKey := filepath.Join(getKbsKubernetesFilePath(), "base/kbs.key")
	policyFile := filepath.Join(trusteeRepoPath, "kbs/sample_policies", customizedOpaFile)
	log.Info("EnableKbsCustomizedPolicy: ", policyFile)
	cmd := exec.Command("./kbs-client", "--url", p.endpoint, "config", "--auth-private-key", privateKey, "set-attestation-policy", "--policy-file", policyFile)
	cmd.Dir = kbsClientDir
	cmd.Env = os.Environ()
	stdoutStderr, err := cmd.CombinedOutput()
	log.Tracef("%v, output: %s", cmd, stdoutStderr)
	if err != nil {
		return err
	}
	return nil
}

func (p *KeyBrokerService) SetSampleSecretKey() error {
	kbsClientDir := filepath.Join(trusteeRepoPath, "target/release")
	privateKey := filepath.Join(getKbsKubernetesFilePath(), "base/kbs.key")
	overlaysPath, err := getOverlaysPath()
	if err != nil {
		return err
	}
	keyFilePath := filepath.Join(getKbsKubernetesFilePath(), overlaysPath, "key.bin")
	log.Info("set key resource: ", keyFilePath)
	cmd := exec.Command("./kbs-client", "--url", p.endpoint, "config", "--auth-private-key", privateKey, "set-resource", "--path", "reponame/workload_key/key.bin", "--resource-file", keyFilePath)
	cmd.Dir = kbsClientDir
	cmd.Env = os.Environ()
	stdoutStderr, err := cmd.CombinedOutput()
	log.Tracef("%v, output: %s", cmd, stdoutStderr)
	if err != nil {
		return err
	}
	return nil
}

func (p *KeyBrokerService) Deploy(ctx context.Context, cfg *envconf.Config, props map[string]string) error {
	log.Info("Customize the overlay yaml file")
	if err := p.installOverlay.Edit(ctx, cfg, props); err != nil {
		return err
	}

	// Create kustomize pointer for overlay directory with updated changes
	tmpoverlay, err := NewKbsInstallOverlay(trusteeRepoPath)
	if err != nil {
		return err
	}

	log.Info("Install Kbs")
	if err := tmpoverlay.Apply(ctx, cfg); err != nil {
		return err
	}
	return nil
}

func (p *KeyBrokerService) Delete(ctx context.Context, cfg *envconf.Config) error {
	// Create kustomize pointer for overlay directory with updated changes
	tmpoverlay, err := NewKbsInstallOverlay(trusteeRepoPath)
	if err != nil {
		return err
	}

	log.Info("Uninstall the cloud-api-adaptor")
	if err = tmpoverlay.Delete(ctx, cfg); err != nil {
		return err
	}
	return nil
}
