package libraryinputresources

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
)

// TODO this is a good target to move to library-go so we all agree how to reference these.
type Resource struct {
	Filename     string
	ResourceType schema.GroupVersionResource
	Content      *unstructured.Unstructured
}

func (r Resource) ID() string {
	name := r.Content.GetName()
	namespace := r.Content.GetNamespace()
	if namespace == "" {
		namespace = "_cluster_scoped_resource_"
	}
	return fmt.Sprintf("%s/%s/%s/%s", r.ResourceType.Group, r.ResourceType.Resource, namespace, name)
}

func LenientResourcesFromDirRecursive(location string) ([]*Resource, error) {
	discoveryClient, err := NewDiscoveryClientFromMustGather(location)
	if err != nil {
		return nil, err
	}

	_, apiResourceList, _, err := discoveryClient.GroupsAndMaybeResources()
	if err != nil {
		return nil, fmt.Errorf("failed to get api resource list: %w", err)
	}

	currResourceList := []*Resource{}
	errs := []error{}
	err = filepath.WalkDir(location, func(currLocation string, currFile fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, err)
		}

		if currFile.IsDir() {
			return nil
		}
		if !strings.HasSuffix(currFile.Name(), ".yaml") && !strings.HasSuffix(currFile.Name(), ".json") {
			return nil
		}
		currResource, err := ResourcesFromFile(apiResourceList, currLocation, location)
		if err != nil {
			return fmt.Errorf("error deserializing %q: %w", currLocation, err)
		}
		currResourceList = append(currResourceList, currResource...)

		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}

	return currResourceList, errors.Join(errs...)
}

func findGVR(resources map[schema.GroupVersion]*metav1.APIResourceList, gvk schema.GroupVersionKind) (*schema.GroupVersionResource, error) {
	apiResourceList, ok := resources[gvk.GroupVersion()]
	if !ok {
		return nil, fmt.Errorf("failed to find api resource list for gvk %s", gvk)
	}

	var matches []*schema.GroupVersionResource
	for _, apiResource := range apiResourceList.APIResources {
		if strings.Contains(apiResource.Name, "/") {
			// Skip subresources
			continue
		}
		if apiResource.Kind == gvk.Kind {
			matches = append(matches, &schema.GroupVersionResource{
				Group:    gvk.Group,
				Version:  gvk.Version,
				Resource: apiResource.Name,
			})
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("failed to find resource for gvk %s", gvk)
	default:
		return nil, fmt.Errorf("multiple resources found for gvk %v", matches)
	}
}

func ResourcesFromFile(apiResourceList map[schema.GroupVersion]*metav1.APIResourceList, location, fileTrimPrefix string) ([]*Resource, error) {
	content, err := os.ReadFile(location)
	if err != nil {
		return nil, fmt.Errorf("unable to read %q: %w", location, err)
	}

	ret, _, jsonErr := unstructured.UnstructuredJSONScheme.Decode(content, nil, &unstructured.Unstructured{})
	if jsonErr != nil {
		// try to see if it's yaml
		jsonString, err := yaml.YAMLToJSON(content)
		if err != nil {
			return nil, fmt.Errorf("unable to decode %q as json: %w", location, jsonErr)
		}
		ret, _, err = unstructured.UnstructuredJSONScheme.Decode(jsonString, nil, &unstructured.Unstructured{})
		if err != nil {
			return nil, fmt.Errorf("unable to decode %q as yaml: %w", location, err)
		}
	}

	retFilename := strings.TrimPrefix(location, fileTrimPrefix)
	retFilename = strings.TrimPrefix(retFilename, "/")
	retContent := ret.(*unstructured.Unstructured)

	resource := &Resource{
		Filename: retFilename,
		Content:  retContent,
	}

	// Short-circuit if the file contains a single resource
	if !resource.Content.IsList() {
		gvk := retContent.GroupVersionKind()
		gvr, err := findGVR(apiResourceList, gvk)
		if err != nil {
			return nil, fmt.Errorf("failed to find gvr: %w", err)
		}
		resource.ResourceType = *gvr
		return []*Resource{resource}, nil
	}

	// Unpack if the file contains a list of resources
	list, err := resource.Content.ToList()
	if err != nil {
		return nil, fmt.Errorf("unable to convert resource content to list: %w", err)
	}

	resources := make([]*Resource, 0, len(list.Items))
	for _, item := range list.Items {
		gvr, err := findGVR(apiResourceList, item.GroupVersionKind())
		if err != nil {
			return nil, fmt.Errorf("failed to find gvr: %w", err)
		}
		resources = append(resources, &Resource{
			Filename:     resource.Filename,
			Content:      &item,
			ResourceType: *gvr,
		})
	}

	return resources, nil
}

func IdentifyResource(in *Resource) string {
	gvkString := fmt.Sprintf("%s.%s.%s/%s[%s]", in.Content.GroupVersionKind().Kind, in.Content.GroupVersionKind().Version, in.Content.GroupVersionKind().Group, in.Content.GetName(), in.Content.GetNamespace())

	return fmt.Sprintf("%s(%s)", gvkString, in.Filename)
}

func WriteResource(in *Resource, parentDir string) error {
	if len(in.Filename) == 0 {
		return fmt.Errorf("%s is missing filename", IdentifyResource(in))
	}

	dir := path.Join(parentDir, path.Dir(in.Filename))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("error creating dir for %v: %w", IdentifyResource(in), err)
	}

	file := path.Join(parentDir, in.Filename)
	resourceYaml, err := yaml.Marshal(in.Content)
	if err != nil {
		return fmt.Errorf("error serializing %v: %w", IdentifyResource(in), err)
	}
	if err := os.WriteFile(file, resourceYaml, 0644); err != nil {
		return fmt.Errorf("error writing %v: %w", IdentifyResource(in), err)
	}

	return nil
}

func EquivalentResources(field string, lhses, rhses []*Resource) []string {
	reasons := []string{}

	for i := range lhses {
		lhs := lhses[i]
		rhs := findResource(rhses, lhs.Filename)

		if rhs == nil {
			reasons = append(reasons, fmt.Sprintf("%v[%d]: %q missing in rhs", field, i, lhs.Filename))
			continue
		}
		if !reflect.DeepEqual(lhs.Content, rhs.Content) {
			reasons = append(reasons, fmt.Sprintf("%v[%d]: does not match: %v", field, i, cmp.Diff(lhs.Content, rhs.Content)))
		}
	}

	for i := range rhses {
		rhs := rhses[i]
		lhs := findResource(lhses, rhs.Filename)

		if lhs == nil {
			reasons = append(reasons, fmt.Sprintf("%v[%d]: %q missing in lhs", field, i, rhs.Filename))
			continue
		}
	}

	return reasons
}

func findResource(in []*Resource, filename string) *Resource {
	for _, curr := range in {
		if curr.Filename == filename {
			return curr
		}
	}

	return nil
}

func NewUniqueResourceSet(resources ...*Resource) *UniqueResourceSet {
	u := &UniqueResourceSet{
		seen:      sets.New[string](),
		resources: []*Resource{},
	}
	u.Insert(resources...)
	return u
}

type UniqueResourceSet struct {
	seen      sets.Set[string]
	resources []*Resource
}

func (u *UniqueResourceSet) Insert(resources ...*Resource) {
	for _, resource := range resources {
		if resource == nil {
			continue
		}
		if u.seen.Has(resource.ID()) {
			continue
		}
		u.resources = append(u.resources, resource)
		u.seen.Insert(resource.ID())
	}
}

func (u *UniqueResourceSet) List() []*Resource {
	return u.resources
}
