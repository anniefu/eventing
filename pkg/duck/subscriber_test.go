/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package duck

import (
	"context"
	"fmt"
	"testing"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	duckv1alpha1 "knative.dev/pkg/apis/duck/v1alpha1"

	eventingv1alpha1 "knative.dev/eventing/pkg/apis/eventing/v1alpha1"
	"knative.dev/eventing/pkg/apis/messaging/v1alpha1"

	fakedynamicclient "knative.dev/pkg/injection/clients/dynamicclient/fake"
)

var (
	uri = "http://example.com"

	channelAddress = "test-channel.hostname"
	channelURL     = fmt.Sprintf("http://%s", channelAddress)
)

func init() {
	// Add types to scheme
	_ = eventingv1alpha1.AddToScheme(scheme.Scheme)
	_ = duckv1alpha1.AddToScheme(scheme.Scheme)
}

func TestDomainToURL(t *testing.T) {
	d := "default-broker.default.svc.cluster.local"
	e := fmt.Sprintf("http://%s/", d)
	if actual := DomainToURL(d); e != actual {
		t.Fatalf("Unexpected domain. Expected '%v', actually '%v'", e, actual)
	}
}

func TestResourceInterface_BadDynamicInterface(t *testing.T) {
	actual, err := ResourceInterface(&badDynamicInterface{}, testNS, schema.GroupVersionKind{})
	if err.Error() != "failed to create dynamic client resource" {
		t.Fatalf("Unexpected error '%v'", err)
	}
	if actual != nil {
		t.Fatalf("Unexpected actual. Expected nil. Actual '%v'", actual)
	}
}

type badDynamicInterface struct{}

var _ dynamic.Interface = &badDynamicInterface{}

func (badDynamicInterface) Resource(_ schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return nil
}

func TestObjectReference_BadDynamicInterface(t *testing.T) {
	actual, err := ObjectReference(context.TODO(), &badDynamicInterface{}, testNS, &corev1.ObjectReference{})
	if err.Error() != "failed to create dynamic client resource" {
		t.Fatalf("Unexpected error '%v'", err)
	}
	if actual != nil {
		t.Fatalf("Unexpected actual. Expected nil. Actual '%v'", actual)
	}
}

func TestSubscriberSpec(t *testing.T) {
	testCases := map[string]struct {
		Sub         *v1alpha1.SubscriberSpec
		Objects     []runtime.Object
		Expected    string
		ExpectedErr string
	}{
		"nil": {
			Sub:      nil,
			Expected: "",
		},
		"empty": {
			Sub:      &v1alpha1.SubscriberSpec{},
			Expected: "",
		},
		"DNS Name": {
			Sub: &v1alpha1.SubscriberSpec{
				DeprecatedDNSName: &uri,
			},
			Expected: uri,
		},
		"URI": {
			Sub: &v1alpha1.SubscriberSpec{
				URI: &uri,
			},
			Expected: uri,
		},
		"Doesn't exist": {
			Sub: &v1alpha1.SubscriberSpec{
				Ref: &corev1.ObjectReference{
					APIVersion: "v1",
					Kind:       "Service",
					Name:       "doesnt-exist",
				},
			},
			ExpectedErr: "services \"doesnt-exist\" not found",
		},
		"K8s Service": {
			Sub: &v1alpha1.SubscriberSpec{
				Ref: &corev1.ObjectReference{
					APIVersion: "v1",
					Kind:       "Service",
					Name:       "does-exist",
				},
			},
			Objects: []runtime.Object{
				k8sService("does-exist"),
			},
			Expected: fmt.Sprintf("http://does-exist.%s.svc.cluster.local/", testNS),
		},
		"Addressable": {
			Sub: &v1alpha1.SubscriberSpec{
				Ref: &corev1.ObjectReference{
					APIVersion: "eventing.knative.dev/v1alpha1",
					Kind:       "Channel",
					Name:       "does-exist",
					Namespace:  testNS,
				},
			},
			Objects: []runtime.Object{
				channel("does-exist"),
			},
			Expected: channelURL,
		},
		"Non-Addressable": {
			Sub: &v1alpha1.SubscriberSpec{
				Ref: &corev1.ObjectReference{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "does-exist",
				},
			},
			Objects: []runtime.Object{
				configMap("does-exist"),
			},
			ExpectedErr: "status does not contain address",
		},
	}

	for n, tc := range testCases {
		t.Run(n, func(t *testing.T) {
			ctx, dc := fakedynamicclient.With(context.Background(), scheme.Scheme, tc.Objects...)
			addressableTracker := NewListableTracker(ctx, &duckv1alpha1.AddressableType{}, func(types.NamespacedName) {}, 0)
			// Not using the testing package due to a cyclic dependency.
			sub := &v1alpha1.Subscription{
				ObjectMeta: v1.ObjectMeta{
					Name:      "subname",
					Namespace: testNS,
				},
				Spec: v1alpha1.SubscriptionSpec{
					Subscriber: tc.Sub,
				},
			}
			track := addressableTracker.TrackInNamespace(sub)
			actual, err := SubscriberSpec(ctx, dc, testNS, tc.Sub, addressableTracker, track)
			if err != nil {
				if tc.ExpectedErr == "" || tc.ExpectedErr != err.Error() {
					t.Fatalf("Unexpected error. Expected '%s'. Actual '%s'.", tc.ExpectedErr, err.Error())
				}
			}
			if tc.Expected != actual {
				t.Fatalf("Unexpected URL. Expected '%s'. Actual '%s'", tc.Expected, actual)
			}
		})
	}
}

func k8sService(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"namespace": testNS,
				"name":      name,
			},
		},
	}
}

func channel(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "eventing.knative.dev/v1alpha1",
			"kind":       "Channel",
			"metadata": map[string]interface{}{
				"namespace": testNS,
				"name":      name,
			},
			"status": map[string]interface{}{
				"address": map[string]interface{}{
					"hostname": channelAddress,
				},
			},
		},
	}
}

func configMap(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"namespace": testNS,
				"name":      name,
			},
		},
	}
}
