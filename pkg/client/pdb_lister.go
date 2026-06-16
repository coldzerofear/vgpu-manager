package client

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/policy/v1"
	listerv1beta1 "k8s.io/client-go/listers/policy/v1beta1"
	"k8s.io/client-go/tools/cache"
)

var conversionScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(policyv1.AddToScheme(conversionScheme))
	utilruntime.Must(policyv1beta1.AddToScheme(conversionScheme))
}

func ConvertPDBV1Beta1ToV1(old *policyv1beta1.PodDisruptionBudget) (*policyv1.PodDisruptionBudget, error) {
	new := &policyv1.PodDisruptionBudget{}
	if err := conversionScheme.Convert(old, new, nil); err != nil {
		return nil, fmt.Errorf("scheme convert failed: %w", err)
	}
	return new, nil
}

func GetSupportedPDBVersion(kubeClient kubernetes.Interface) (string, error) {
	// priority check policy/v1（K8s 1.21+ GA）
	resourceList, err := kubeClient.Discovery().ServerResourcesForGroupVersion("policy/v1")
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("failed to discover policy/v1: %w", err)
	}
	if resourceList != nil {
		for _, r := range resourceList.APIResources {
			if r.Name == "poddisruptionbudgets" && r.Kind == "PodDisruptionBudget" {
				return "v1", nil
			}
		}
	}

	// fallback policy/v1beta1（K8s 1.5 ~ 1.20）
	resourceList, err = kubeClient.Discovery().ServerResourcesForGroupVersion("policy/v1beta1")
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("failed to discover policy/v1beta1: %w", err)
	}
	if resourceList != nil {
		for _, r := range resourceList.APIResources {
			if r.Name == "poddisruptionbudgets" && r.Kind == "PodDisruptionBudget" {
				return "v1beta1", nil
			}
		}
	}
	return "", fmt.Errorf("PodDisruptionBudget not found in policy/v1 or policy/v1beta1")
}

// podLister implements the PodLister interface.
type pdbLister struct {
	v1      listerv1.PodDisruptionBudgetLister
	v1beta1 listerv1beta1.PodDisruptionBudgetLister
}

// NewPDBLister returns a new PDBLister.
func NewPDBLister(client kubernetes.Interface, factory informers.SharedInformerFactory) (listerv1.PodDisruptionBudgetLister, cache.SharedIndexInformer, error) {
	version, err := GetSupportedPDBVersion(client)
	if err != nil {
		return nil, nil, err
	}
	lister := &pdbLister{}
	var informer cache.SharedIndexInformer
	switch version {
	case "v1":
		informer = factory.Policy().V1().PodDisruptionBudgets().Informer()
		lister.v1 = factory.Policy().V1().PodDisruptionBudgets().Lister()
	case "v1beta1":
		informer = factory.Policy().V1beta1().PodDisruptionBudgets().Informer()
		lister.v1beta1 = factory.Policy().V1beta1().PodDisruptionBudgets().Lister()
	default:
		return nil, nil, fmt.Errorf("unsupported PodDisruptionBudget version: %s", version)
	}
	return lister, informer, nil
}

func (p *pdbLister) List(selector labels.Selector) ([]*policyv1.PodDisruptionBudget, error) {
	switch {
	case p.v1 != nil:
		return p.v1.List(selector)
	case p.v1beta1 != nil:
		pdbs, err := p.v1beta1.List(selector)
		if err != nil {
			return nil, err
		}
		ret := make([]*policyv1.PodDisruptionBudget, len(pdbs))
		for i, budget := range pdbs {
			ret[i], err = ConvertPDBV1Beta1ToV1(budget)
			if err != nil {
				return nil, err
			}
		}
		return ret, nil
	default:
		return nil, fmt.Errorf("unsupported PodDisruptionBudget version")
	}
}

func (p *pdbLister) PodDisruptionBudgets(namespace string) listerv1.PodDisruptionBudgetNamespaceLister {
	switch {
	case p.v1 != nil:
		return p.v1.PodDisruptionBudgets(namespace)
	case p.v1beta1 != nil:
		return &podDisruptionBudgetNamespaceLister{
			lister: p.v1beta1.PodDisruptionBudgets(namespace),
		}
	default:
		return nil
	}
}

type podDisruptionBudgetNamespaceLister struct {
	lister listerv1beta1.PodDisruptionBudgetNamespaceLister
}

func (p *podDisruptionBudgetNamespaceLister) List(selector labels.Selector) ([]*policyv1.PodDisruptionBudget, error) {
	pdbs, err := p.lister.List(selector)
	if err != nil {
		return nil, err
	}
	ret := make([]*policyv1.PodDisruptionBudget, len(pdbs))
	for i, budget := range pdbs {
		ret[i], err = ConvertPDBV1Beta1ToV1(budget)
		if err != nil {
			return nil, err
		}
	}
	return ret, nil
}

func (p *podDisruptionBudgetNamespaceLister) Get(name string) (*policyv1.PodDisruptionBudget, error) {
	pdb, err := p.lister.Get(name)
	if err != nil {
		return nil, err
	}
	return ConvertPDBV1Beta1ToV1(pdb)
}

func (p *pdbLister) GetPodPodDisruptionBudgets(pod *corev1.Pod) ([]*policyv1.PodDisruptionBudget, error) {
	switch {
	case p.v1 != nil:
		return p.v1.GetPodPodDisruptionBudgets(pod)
	case p.v1beta1 != nil:
		pdbs, err := p.v1beta1.GetPodPodDisruptionBudgets(pod)
		if err != nil {
			return nil, err
		}
		ret := make([]*policyv1.PodDisruptionBudget, len(pdbs))
		for i, budget := range pdbs {
			ret[i], err = ConvertPDBV1Beta1ToV1(budget)
			if err != nil {
				return nil, err
			}
		}
		return ret, nil
	default:
		return nil, fmt.Errorf("unsupported PodDisruptionBudget version")
	}
}
