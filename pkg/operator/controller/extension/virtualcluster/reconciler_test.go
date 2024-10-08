// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package virtualcluster_test

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	testclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gardencorev1 "github.com/gardener/gardener/pkg/apis/core/v1"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	operatorv1alpha1 "github.com/gardener/gardener/pkg/apis/operator/v1alpha1"
	fakeclientmap "github.com/gardener/gardener/pkg/client/kubernetes/clientmap/fake"
	"github.com/gardener/gardener/pkg/client/kubernetes/clientmap/keys"
	"github.com/gardener/gardener/pkg/operator/apis/config"
	operatorclient "github.com/gardener/gardener/pkg/operator/client"
	. "github.com/gardener/gardener/pkg/operator/controller/extension/virtualcluster"
	"github.com/gardener/gardener/pkg/utils/test/matchers"
)

const (
	gardenName    = "garden"
	extensionName = "extension"
)

var _ = Describe("Reconciler", func() {
	var (
		runtimeClient   client.Client
		virtualClient   client.Client
		ctx             context.Context
		operatorConfig  config.OperatorConfiguration
		gardenClientMap *fakeclientmap.ClientMap
		reconciler      *Reconciler
		garden          *operatorv1alpha1.Garden
		extension       *operatorv1alpha1.Extension
		fakeClock       *testclock.FakeClock
		fakeRecorder    *record.FakeRecorder
	)

	BeforeEach(func() {
		ctx = context.Background()
		logf.IntoContext(ctx, logr.Discard())

		operatorConfig = config.OperatorConfiguration{
			Controllers: config.ControllerConfiguration{
				ExtensionVirtualCluster: config.ExtensionVirtualClusterControllerConfiguration{
					ConcurrentSyncs: ptr.To(1),
				},
			},
		}
		garden = &operatorv1alpha1.Garden{
			ObjectMeta: metav1.ObjectMeta{
				Name: gardenName,
			},
		}
		extension = &operatorv1alpha1.Extension{
			ObjectMeta: metav1.ObjectMeta{
				Name: extensionName,
			},
			Spec: operatorv1alpha1.ExtensionSpec{
				Resources: []gardencorev1beta1.ControllerResource{
					{Kind: "Worker"},
				},
				Deployment: &operatorv1alpha1.Deployment{
					ExtensionDeployment: &operatorv1alpha1.ExtensionDeploymentSpec{
						DeploymentSpec: operatorv1alpha1.DeploymentSpec{
							Helm: &operatorv1alpha1.ExtensionHelm{
								OCIRepository: &gardencorev1.OCIRepository{
									Ref: ptr.To("removeFinalizer"),
								},
							},
						},
					},
				},
			},
		}

		virtualClient = fakeclient.NewClientBuilder().WithScheme(operatorclient.VirtualScheme).Build()
		runtimeClient = fakeclient.NewClientBuilder().
			WithScheme(operatorclient.RuntimeScheme).
			WithStatusSubresource(&operatorv1alpha1.Extension{}, &operatorv1alpha1.Extension{}).Build()
		gardenClientMap = fakeclientmap.NewClientMapBuilder().WithRuntimeClientForKey(keys.ForGarden(garden), virtualClient, nil).Build()

		fakeClock = testclock.NewFakeClock(time.Now())
		fakeRecorder = &record.FakeRecorder{}
	})

	Describe("#ExtensionVirtualCluster", func() {
		var req reconcile.Request

		BeforeEach(func() {
			req = reconcile.Request{NamespacedName: client.ObjectKey{Name: extensionName}}
		})

		JustBeforeEach(func() {
			Expect(runtimeClient.Create(ctx, extension)).To(Succeed())
			reconciler = &Reconciler{
				Config:          operatorConfig,
				GardenClientMap: gardenClientMap,
				RuntimeClient:   runtimeClient,
				Clock:           fakeClock,
				Recorder:        fakeRecorder,
			}
		})

		Context("when extension no longer exists", func() {
			It("should stop reconciling and not requeue", func() {
				req = reconcile.Request{NamespacedName: client.ObjectKey{Name: "some-random-extension"}}
				Expect(reconciler.Reconcile(ctx, req)).To(Equal(reconcile.Result{}))
			})
		})

		Context("reconcile based on garden state", func() {
			It("remove finalizers when garden is deleted", func() {
				extension.Finalizers = append(extension.Finalizers, operatorv1alpha1.FinalizerName)
				Expect(runtimeClient.Update(ctx, extension)).To(Succeed())

				req = reconcile.Request{NamespacedName: client.ObjectKey{Name: extensionName}}
				Expect(reconciler.Reconcile(ctx, req)).To(Equal(reconcile.Result{}))

				extension := &operatorv1alpha1.Extension{}
				Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: extensionName}, extension)).To(Succeed())
				Expect(extension.Finalizers).To(BeEmpty())
			})

			It("should requeue if gardener is not ready", func() {
				garden.Status = operatorv1alpha1.GardenStatus{
					LastOperation: &gardencorev1beta1.LastOperation{
						State: gardencorev1beta1.LastOperationStateProcessing,
						Type:  gardencorev1beta1.LastOperationTypeReconcile,
					},
				}
				Expect(runtimeClient.Create(ctx, garden)).To(Succeed())

				res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Name: extensionName}})
				Expect(res).To(Equal(reconcile.Result{RequeueAfter: 10 * time.Second}))
				Expect(err).NotTo(HaveOccurred())
			})

			It("should create the controller-{registration,deployment} if garden is ready", func() {
				garden.Status = operatorv1alpha1.GardenStatus{
					LastOperation: &gardencorev1beta1.LastOperation{
						State: gardencorev1beta1.LastOperationStateSucceeded,
						Type:  gardencorev1beta1.LastOperationTypeReconcile,
					},
				}
				Expect(runtimeClient.Create(ctx, garden)).To(Succeed())

				req = reconcile.Request{NamespacedName: client.ObjectKey{Name: extensionName}}
				Expect(reconciler.Reconcile(ctx, req)).To(Equal(reconcile.Result{}))

				extension := &operatorv1alpha1.Extension{}
				Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: extensionName}, extension)).To(Succeed())
				Expect(extension.Status.Conditions).To(HaveLen(1))
				Expect(extension.Status.Conditions[0]).To(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{
					"Type":   Equal(operatorv1alpha1.VirtualClusterExtensionReconciled),
					"Status": Equal(gardencorev1beta1.ConditionTrue),
					"Reason": Equal(ConditionReconcileSuccess),
				}))

				var (
					controllerDeploymentList   gardencorev1.ControllerDeploymentList
					controllerRegistrationList gardencorev1beta1.ControllerRegistrationList
				)
				Expect(virtualClient.List(ctx, &controllerRegistrationList)).To(Succeed())
				Expect(virtualClient.List(ctx, &controllerDeploymentList)).To(Succeed())
				Expect(controllerDeploymentList.Items).To(HaveLen(1))
				Expect(controllerRegistrationList.Items).To(HaveLen(1))

				Expect(runtimeClient.Delete(ctx, extension)).To(Succeed())
				req = reconcile.Request{NamespacedName: client.ObjectKey{Name: extensionName}}
				Expect(reconciler.Reconcile(ctx, req)).To(Equal(reconcile.Result{}))

				Expect(runtimeClient.Get(ctx, client.ObjectKey{Name: extensionName}, extension)).To(matchers.BeNotFoundError())
				Expect(virtualClient.List(ctx, &controllerRegistrationList)).To(Succeed())
				Expect(virtualClient.List(ctx, &controllerDeploymentList)).To(Succeed())
				Expect(controllerDeploymentList.Items).To(BeEmpty())
				Expect(controllerRegistrationList.Items).To(BeEmpty())
			})
		})
	})
})
