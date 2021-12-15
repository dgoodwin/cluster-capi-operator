package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	certmangerv1 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	"github.com/pkg/errors"
	admissionregistration "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	configclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
	operatorv1 "sigs.k8s.io/cluster-api/exp/operator/api/v1alpha1"
	utilyaml "sigs.k8s.io/cluster-api/util/yaml"
)

type provider struct {
	name       string
	version    string
	ptype      clusterctlv1.ProviderType
	components repository.Components
	metadata   []byte
}

const (
	sampleImageFileName      = "../sample-images.json"
	providerVersionsFileName = "provider-versions.json"
)

var (
	providers = []provider{
		{name: "cluster-api", ptype: clusterctlv1.CoreProviderType},
		{name: "aws", ptype: clusterctlv1.InfrastructureProviderType},
		{name: "azure", ptype: clusterctlv1.InfrastructureProviderType},
		{name: "metal3", ptype: clusterctlv1.InfrastructureProviderType},
		{name: "gcp", ptype: clusterctlv1.InfrastructureProviderType},
		{name: "openstack", ptype: clusterctlv1.InfrastructureProviderType},
	}
	providersPath = path.Join(projDir, "assets", "providers")
	manifestsPath = path.Join(projDir, "manifests")
)

func (p *provider) loadComponents() error {
	configClient, err := configclient.New("")
	if err != nil {
		return err
	}

	providerConfig, err := configClient.Providers().Get(p.name, p.ptype)
	if err != nil {
		return err
	}

	repo, err := repository.NewGitHubRepository(providerConfig, configClient.Variables())
	if err != nil {
		return err
	}

	err = p.loadVersion()
	if err != nil {
		return err
	}

	p.metadata, err = repo.GetFile(p.version, "metadata.yaml")
	if err != nil {
		return err
	}

	options := repository.ComponentsOptions{
		TargetNamespace:     "openshift-cluster-api",
		SkipTemplateProcess: true,
		Version:             p.version,
	}

	componentsFile, err := repo.GetFile(options.Version, repo.ComponentsPath())
	if err != nil {
		return errors.Wrapf(err, "failed to read %q from provider's repository %q", repo.ComponentsPath(), providerConfig.ManifestLabel())
	}

	ci := repository.ComponentsInput{
		Provider:     providerConfig,
		ConfigClient: configClient,
		Processor:    yamlprocessor.NewSimpleProcessor(),
		RawYaml:      componentsFile,
		Options:      options}

	p.components, err = repository.NewComponents(ci)
	return err
}

func (p *provider) providerTypeName() string {
	return strings.ReplaceAll(strings.ToLower(string(p.ptype)), "provider", "")
}

func (p *provider) writeProviderComponents(objs []unstructured.Unstructured) error {
	combined, err := utilyaml.FromUnstructured(objs)
	if err != nil {
		return err
	}

	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.name + "-" + p.version,
			Namespace: "openshift-cluster-api",
			Labels: map[string]string{
				"provider.cluster.x-k8s.io/name":    p.name,
				"provider.cluster.x-k8s.io/type":    p.providerTypeName(),
				"provider.cluster.x-k8s.io/version": p.version,
			},
		},
		Data: map[string]string{
			"metadata":   string(p.metadata),
			"components": string(combined),
		},
	}

	cmYaml, err := yaml.Marshal(cm)
	if err != nil {
		return err
	}

	fName := strings.ToLower(p.providerTypeName() + "-" + p.name + ".yaml")
	return os.WriteFile(path.Join(providersPath, fName), ensureNewLine(cmYaml), 0600)
}

// ensureNewLine makes sure that there is one new line at the end of the file for git
func ensureNewLine(b []byte) []byte {
	return append(bytes.TrimRight(b, "\n"), []byte("\n")...)
}

func (p *provider) writeRBACComponentsToManifests(objs []unstructured.Unstructured) error {
	combined, err := utilyaml.FromUnstructured(objs)
	if err != nil {
		return err
	}

	fName := strings.ToLower("0000_30_cluster-api-" + p.providerTypeName() + "-" + p.name + "_03_rbac.yaml")
	return os.WriteFile(path.Join(manifestsPath, fName), ensureNewLine(combined), 0600)
}

func (p *provider) writeProviders() error {
	var obj client.Object
	switch p.providerTypeName() {
	case "core":
		obj = &operatorv1.CoreProvider{
			TypeMeta: metav1.TypeMeta{Kind: "CoreProvider", APIVersion: "operator.cluster.x-k8s.io/v1alpha1"},
			Spec:     operatorv1.CoreProviderSpec{ProviderSpec: p.providerSpec()},
		}
	case "controlplane":
		obj = &operatorv1.ControlPlaneProvider{
			TypeMeta: metav1.TypeMeta{Kind: "ControlPlaneProvider", APIVersion: "operator.cluster.x-k8s.io/v1alpha1"},
			Spec:     operatorv1.ControlPlaneProviderSpec{ProviderSpec: p.providerSpec()},
		}
	case "bootstrap":
		obj = &operatorv1.BootstrapProvider{
			TypeMeta: metav1.TypeMeta{Kind: "BootstrapProvider", APIVersion: "operator.cluster.x-k8s.io/v1alpha1"},
			Spec:     operatorv1.BootstrapProviderSpec{ProviderSpec: p.providerSpec()},
		}
	case "infrastructure":
		obj = &operatorv1.InfrastructureProvider{
			TypeMeta: metav1.TypeMeta{Kind: "InfrastructureProvider", APIVersion: "operator.cluster.x-k8s.io/v1alpha1"},
			Spec:     operatorv1.InfrastructureProviderSpec{ProviderSpec: p.providerSpec()},
		}
	}
	obj.SetName(p.name)
	obj.SetNamespace("openshift-cluster-api")

	cmYaml, err := yaml.Marshal(obj)
	if err != nil {
		return err
	}

	fName := strings.ToLower(p.providerTypeName() + "-" + p.name + "-provider.yaml")
	return os.WriteFile(path.Join(providersPath, fName), ensureNewLine(cmYaml), 0600)
}

func (p *provider) providerSpec() operatorv1.ProviderSpec {
	return operatorv1.ProviderSpec{
		Version: &p.version,
		FetchConfig: &operatorv1.FetchConfiguration{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"provider.cluster.x-k8s.io/name": p.name,
					"provider.cluster.x-k8s.io/type": p.providerTypeName(),
				},
			},
		},
	}
}

func findWebhookServiceSecretName(objs []unstructured.Unstructured) map[string]string {
	serviceSecretNames := map[string]string{}
	certSecretNames := map[string]string{}

	secretFromCertNN := func(certNN string) (string, bool) {
		certName := strings.Split(certNN, "/")[1]
		secretName, ok := certSecretNames[certName]
		if !ok || secretName == "" {
			return "", false
		}
		return secretName, true
	}
	// find service, then cert, then secret
	// return map[certName] = secretName
	for i, obj := range objs {
		switch obj.GetKind() {
		case "Certificate":
			cert := &certmangerv1.Certificate{}
			if err := scheme.Convert(&objs[i], cert, nil); err != nil {
				panic(err)
			}
			certSecretNames[cert.Name] = cert.Spec.SecretName
		}
	}
	for _, obj := range objs {
		switch obj.GetKind() {
		case "CustomResourceDefinition":
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := scheme.Convert(&obj, crd, nil); err != nil {
				panic(err)
			}
			if certNN, ok := crd.Annotations["cert-manager.io/inject-ca-from"]; ok {
				secretName, ok := secretFromCertNN(certNN)
				if !ok {
					panic("can't find secret from cert " + certNN)
				}
				serviceSecretNames[crd.Spec.Conversion.Webhook.ClientConfig.Service.Name] = secretName
			}

		case "MutatingWebhookConfiguration":
			mwc := &admissionregistration.MutatingWebhookConfiguration{}
			if err := scheme.Convert(&obj, mwc, nil); err != nil {
				panic(err)
			}
			if certNN, ok := mwc.Annotations["cert-manager.io/inject-ca-from"]; ok {
				secretName, ok := secretFromCertNN(certNN)
				if !ok {
					panic("can't find secret from cert " + certNN)
				}
				serviceSecretNames[mwc.Webhooks[0].ClientConfig.Service.Name] = secretName
			}

		case "ValidatingWebhookConfiguration":
			vwc := &admissionregistration.ValidatingWebhookConfiguration{}
			if err := scheme.Convert(&obj, vwc, nil); err != nil {
				panic(err)
			}
			if certNN, ok := vwc.Annotations["cert-manager.io/inject-ca-from"]; ok {
				secretName, ok := secretFromCertNN(certNN)
				if !ok {
					panic("can't find secret from cert " + certNN)
				}
				serviceSecretNames[vwc.Webhooks[0].ClientConfig.Service.Name] = secretName
			}
		}
	}
	return serviceSecretNames
}

func (p *provider) updateImages(objs []unstructured.Unstructured) error {
	jsonData, err := ioutil.ReadFile(filepath.Clean(sampleImageFileName))
	if err != nil {
		return err
	}
	containerImages := map[string]string{}
	if err := json.Unmarshal(jsonData, &containerImages); err != nil {
		return err
	}

	for i, obj := range objs {
		switch obj.GetKind() {
		case "Deployment":
			dep := &appsv1.Deployment{}
			if err := scheme.Convert(&objs[i], dep, nil); err != nil {
				return err
			}
			for _, c := range dep.Spec.Template.Spec.Containers {
				containerImages[p.imageToKey(c.Image)] = c.Image
			}
		}
	}

	jsonData, err = json.MarshalIndent(&containerImages, "", "  ")
	if err != nil {
		return err
	}

	return ioutil.WriteFile(sampleImageFileName, ensureNewLine(jsonData), 0600)
}

func (p *provider) loadVersion() error {
	jsonData, err := ioutil.ReadFile(providerVersionsFileName)
	if err != nil {
		return err
	}
	providerVersions := map[string]string{}
	if err := json.Unmarshal(jsonData, &providerVersions); err != nil {
		return err
	}

	p.version = providerVersions[p.name]
	return nil
}

func (p *provider) imageToKey(fullImage string) string {
	//k8s.gcr.io/cluster-api/kubeadm-bootstrap-controller:v0.4.3
	frag := strings.Split(fullImage, "/")
	nameVer := frag[len(frag)-1]
	name := strings.Split(nameVer, ":")[0]

	switch name {
	case "kube-rbac-proxy":
		return "kube-rbac-proxy"
	case "ip-address-manager": //special case
		return p.providerTypeName() + "-" + p.name + ":" + name
	default:
		return p.providerTypeName() + "-" + p.name + ":manager"
	}
}

func certManagerToServiceCA(objs []unstructured.Unstructured) []unstructured.Unstructured {
	serviceSecretNames := findWebhookServiceSecretName(objs)

	finalObjs := []unstructured.Unstructured{}
	for _, obj := range objs {
		switch obj.GetKind() {
		case "CustomResourceDefinition", "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration":
			anns := obj.GetAnnotations()
			if anns == nil {
				anns = map[string]string{}
			}
			if _, ok := anns["cert-manager.io/inject-ca-from"]; ok {
				anns["service.beta.openshift.io/inject-cabundle"] = "true"
				delete(anns, "cert-manager.io/inject-ca-from")
				obj.SetAnnotations(anns)
			}
			finalObjs = append(finalObjs, obj)
		case "Service":
			anns := obj.GetAnnotations()
			if anns == nil {
				anns = map[string]string{}
			}
			if name, ok := serviceSecretNames[obj.GetName()]; ok {
				fmt.Println(obj.GetKind(), obj.GetName(), name)
				anns["service.beta.openshift.io/serving-cert-secret-name"] = name
				obj.SetAnnotations(anns)
			}
			finalObjs = append(finalObjs, obj)
		case "Certificate", "Issuer", "Namespace": // skip
		default:
			finalObjs = append(finalObjs, obj)
		}
	}
	return finalObjs
}

func splitRBACOut(objs []unstructured.Unstructured) ([]unstructured.Unstructured, []unstructured.Unstructured) {
	finalObjs := []unstructured.Unstructured{}
	rbacObjs := []unstructured.Unstructured{}
	for _, obj := range objs {
		switch obj.GetKind() {
		case "ClusterRole", "Role", "ClusterRoleBinding", "RoleBinding", "ServiceAccount":
			setOpenShiftAnnotations(obj, false)
			rbacObjs = append(rbacObjs, obj)
		default:
			finalObjs = append(finalObjs, obj)
		}
	}
	return finalObjs, rbacObjs
}

func filterOutIPAM(objs []unstructured.Unstructured) []unstructured.Unstructured {
	finalObjs := []unstructured.Unstructured{}
	for _, obj := range objs {
		if obj.GetKind() == "CustomResourceDefinition" || !strings.Contains(strings.ToLower(obj.GetName()), "ipam") {
			finalObjs = append(finalObjs, obj)
		}
	}
	return finalObjs
}

func importProviders(providerFilter string) error {
	for _, p := range providers {
		if providerFilter != "" && p.name != providerFilter {
			continue
		}

		err := p.loadComponents()
		if err != nil {
			return err
		}
		fmt.Println(p.ptype, p.name)

		finalObjs, rbacObjs := splitRBACOut(certManagerToServiceCA(p.components.Objs()))

		if p.name == "metal3" {
			finalObjs = filterOutIPAM(finalObjs)
		}

		err = p.writeRBACComponentsToManifests(rbacObjs)
		if err != nil {
			return err
		}

		err = p.updateImages(finalObjs)
		if err != nil {
			return err
		}

		err = p.writeProviderComponents(finalObjs)
		if err != nil {
			return err
		}

		err = p.writeProviders()
		if err != nil {
			return err
		}
	}
	return nil
}
