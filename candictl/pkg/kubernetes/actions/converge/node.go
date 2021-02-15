package converge

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/deckhouse/deckhouse/candictl/pkg/kubernetes/actions/deckhouse"
	"github.com/deckhouse/deckhouse/candictl/pkg/kubernetes/client"
	"github.com/deckhouse/deckhouse/candictl/pkg/log"
	"github.com/deckhouse/deckhouse/candictl/pkg/util/retry"
)

var nodeGroupResource = schema.GroupVersionResource{Group: "deckhouse.io", Version: "v1alpha1", Resource: "nodegroups"}

func GetCloudConfig(kubeCl *client.KubernetesClient, nodeGroupName string) (string, error) {
	var cloudData string

	name := fmt.Sprintf("Waiting for %s cloud config️", nodeGroupName)
	err := log.Process("default", name, func() error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
					_, _ = deckhouse.NewLogPrinter(kubeCl).Print(ctx)
				}
			}
		}()

		err := retry.StartSilentLoop(name, 45, 5, func() error {
			secret, err := kubeCl.CoreV1().
				Secrets("d8-cloud-instance-manager").
				Get("manual-bootstrap-for-"+nodeGroupName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			cloudData = base64.StdEncoding.EncodeToString(secret.Data["cloud-config"])
			return nil
		})
		if err != nil {
			return err
		}

		log.InfoLn("Cloud configuration found!")
		return nil
	})
	return cloudData, err
}

func CreateNodeGroup(kubeCl *client.KubernetesClient, nodeGroupName string, data map[string]interface{}) error {
	doc := unstructured.Unstructured{}
	doc.SetUnstructuredContent(data)

	resourceSchema := schema.GroupVersionResource{Group: "deckhouse.io", Version: "v1alpha1", Resource: "nodegroups"}

	return retry.StartLoop(fmt.Sprintf("Create NodeGroup %q", nodeGroupName), 45, 15, func() error {
		res, err := kubeCl.Dynamic().
			Resource(resourceSchema).
			Create(&doc, metav1.CreateOptions{})
		if err == nil {
			log.InfoF("NodeGroup %q created\n", res.GetName())
			return nil
		}

		if errors.IsAlreadyExists(err) {
			log.InfoF("Object %v, updating ... ", err)
			content, err := doc.MarshalJSON()
			if err != nil {
				return err
			}
			_, err = kubeCl.Dynamic().
				Resource(resourceSchema).
				Patch(doc.GetName(), types.MergePatchType, content, metav1.PatchOptions{})
			if err != nil {
				return err
			}
			log.InfoLn("OK!")
		}
		return nil
	})
}

func WaitForSingleNodeBecomeReady(kubeCl *client.KubernetesClient, nodeName string) error {
	return retry.StartLoop(fmt.Sprintf("Waiting for  Node %s to become Ready", nodeName), 100, 20, func() error {
		node, err := kubeCl.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		for _, c := range node.Status.Conditions {
			if c.Type == apiv1.NodeReady {
				if c.Status == apiv1.ConditionTrue {
					return nil
				}
			}
		}

		return fmt.Errorf("node %q is not Ready yet", nodeName)
	})
}

func WaitForNodesBecomeReady(kubeCl *client.KubernetesClient, nodeGroupName string, desiredReadyNodes int) error {
	return retry.StartLoop(fmt.Sprintf("Waiting for NodeGroup %s to become Ready", nodeGroupName), 100, 20, func() error {
		nodes, err := kubeCl.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: "node.deckhouse.io/group=" + nodeGroupName})
		if err != nil {
			return err
		}

		readyNodes := make(map[string]struct{})

		for _, node := range nodes.Items {
			for _, c := range node.Status.Conditions {
				if c.Type == apiv1.NodeReady {
					if c.Status == apiv1.ConditionTrue {
						readyNodes[node.Name] = struct{}{}
					}
				}
			}
		}

		message := fmt.Sprintf("Nodes Ready %v of %v\n", len(readyNodes), desiredReadyNodes)
		for _, node := range nodes.Items {
			condition := "NotReady"
			if _, ok := readyNodes[node.Name]; ok {
				condition = "Ready"
			}
			message += fmt.Sprintf("* %s | %s\n", node.Name, condition)
		}

		if len(readyNodes) >= desiredReadyNodes {
			log.InfoLn(message)
			return nil
		}

		return fmt.Errorf(strings.TrimSuffix(message, "\n"))
	})
}

func WaitForNodesListBecomeReady(kubeCl *client.KubernetesClient, nodes []string) error {
	return retry.StartLoop("Waiting for nodes to become Ready", 100, 20, func() error {
		desiredReadyNodes := len(nodes)
		var nodesList apiv1.NodeList

		for _, nodeName := range nodes {
			node, err := kubeCl.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			nodesList.Items = append(nodesList.Items, *node)
		}

		readyNodes := make(map[string]struct{})

		for _, node := range nodesList.Items {
			for _, c := range node.Status.Conditions {
				if c.Type == apiv1.NodeReady {
					if c.Status == apiv1.ConditionTrue {
						readyNodes[node.Name] = struct{}{}
					}
				}
			}
		}

		message := fmt.Sprintf("Nodes Ready %v of %v\n", len(readyNodes), desiredReadyNodes)
		for _, node := range nodesList.Items {
			condition := "NotReady"
			if _, ok := readyNodes[node.Name]; ok {
				condition = "Ready"
			}
			message += fmt.Sprintf("* %s | %s\n", node.Name, condition)
		}

		if len(readyNodes) >= desiredReadyNodes {
			log.InfoLn(message)
			return nil
		}

		return fmt.Errorf(strings.TrimSuffix(message, "\n"))
	})
}

func GetNodeGroupTemplates(kubeCl *client.KubernetesClient) (map[string]map[string]interface{}, error) {
	nodeTemplates := make(map[string]map[string]interface{})

	err := retry.StartLoop("Get NodeGroups node template settings", 10, 5, func() error {
		nodeGroups, err := kubeCl.Dynamic().Resource(nodeGroupResource).List(metav1.ListOptions{})
		if err != nil {
			return err
		}

		for _, group := range nodeGroups.Items {
			var nodeTemplate map[string]interface{}
			if spec, ok := group.Object["spec"].(map[string]interface{}); ok {
				nodeTemplate, _ = spec["nodeTemplate"].(map[string]interface{})
			}

			nodeTemplates[group.GetName()] = nodeTemplate
		}
		return nil
	})

	return nodeTemplates, err
}

func DeleteNode(kubeCl *client.KubernetesClient, nodeName string) error {
	return retry.StartLoop(fmt.Sprintf("Delete Node %s", nodeName), 45, 10, func() error {
		err := kubeCl.CoreV1().Nodes().Delete(nodeName, &metav1.DeleteOptions{})
		if errors.IsNotFound(err) {
			// Node has already been deleted
			return nil
		}
		return err
	})
}

func DeleteNodeGroup(kubeCl *client.KubernetesClient, nodeGroupName string) error {
	return retry.StartLoop(fmt.Sprintf("Delete NodeGroup %s", nodeGroupName), 45, 10, func() error {
		err := kubeCl.Dynamic().Resource(nodeGroupResource).Delete(nodeGroupName, &metav1.DeleteOptions{})
		if errors.IsNotFound(err) {
			// NodeGroup has already been deleted
			return nil
		}
		return err
	})
}
