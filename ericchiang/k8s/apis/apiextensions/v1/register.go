package v1

import "neuvector/ericchiang/k8s"

func init() {
	k8s.Register("apiextensions.k8s.io", "v1", "customresourcedefinitions", false, &CustomResourceDefinition{})

	k8s.RegisterList("apiextensions.k8s.io", "v1", "customresourcedefinitions", false, &CustomResourceDefinitionList{})
}
