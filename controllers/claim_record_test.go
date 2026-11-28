/*
Copyright 2026 The Beskar7 Authors.

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

package controllers

import (
	"context"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/beskar7/beskar7/api/v1beta2"
)

// staleHostView serves PhysicalHost reads from a snapshot, like a cache that lags behind.
type staleHostView struct {
	namespace string
	hosts     []infrav1.PhysicalHost
	// reversed flips the list order, since an informer's list has none.
	reversed bool
}

func (v *staleHostView) catchUp() {
	list := &infrav1.PhysicalHostList{}
	Expect(k8sClient.List(ctx, list, client.InNamespace(v.namespace))).To(Succeed())
	v.hosts = list.Items
}

func (v *staleHostView) client() client.WithWatch {
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			host, ok := obj.(*infrav1.PhysicalHost)
			if !ok {
				return c.Get(ctx, key, obj, opts...)
			}
			for i := range v.hosts {
				if client.ObjectKeyFromObject(&v.hosts[i]) == key {
					v.hosts[i].DeepCopyInto(host)
					return nil
				}
			}
			return apierrors.NewNotFound(infrav1.GroupVersion.WithResource("physicalhosts").GroupResource(), key.Name)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			hosts, ok := list.(*infrav1.PhysicalHostList)
			if !ok {
				return c.List(ctx, list, opts...)
			}
			hosts.Items = v.matching(opts)
			return nil
		},
	})
}

func (v *staleHostView) matching(opts []client.ListOption) []infrav1.PhysicalHost {
	lo := (&client.ListOptions{}).ApplyOptions(opts)
	var out []infrav1.PhysicalHost
	for i := range v.hosts {
		host := &v.hosts[i]
		if lo.Namespace != "" && host.Namespace != lo.Namespace {
			continue
		}
		if lo.LabelSelector != nil && !lo.LabelSelector.Matches(labels.Set(host.Labels)) {
			continue
		}
		if lo.FieldSelector != nil && !lo.FieldSelector.Matches(hostIndexFields(host)) {
			continue
		}
		out = append(out, *host.DeepCopy())
	}
	if v.reversed {
		slices.Reverse(out)
	}
	return out
}

func hostIndexFields(host *infrav1.PhysicalHost) fields.Set {
	set := fields.Set{PhysicalHostStateIndex: string(host.Status.State)}
	if consumer := physicalHostConsumer(host); len(consumer) > 0 {
		set[PhysicalHostConsumerIndex] = consumer[0]
	}
	return set
}

// lostPatchAnswer applies the first PhysicalHost patch but reports it as failed.
func lostPatchAnswer(c client.WithWatch) client.WithWatch {
	lost := false
	return interceptor.NewClient(c, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := cl.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			if _, isHost := obj.(*infrav1.PhysicalHost); !isHost || lost {
				return nil
			}
			lost = true
			return apierrors.NewServerTimeout(infrav1.GroupVersion.WithResource("physicalhosts").GroupResource(), "patch", 1)
		},
	})
}

func failingHostGets() client.Reader {
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	return failGetOf(base, &infrav1.PhysicalHost{})
}

var _ = Describe("A claim the cache has not seen yet", func() {
	var (
		ns   *corev1.Namespace
		view *staleHostView
		b7m  *infrav1.Beskar7Machine
	)

	createAvailableHost := func(name string) {
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns.Name,
			},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://192.168.100.1",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		host.Status.State = infrav1.StateAvailable
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	}

	machineReconciler := func(c client.Client) *Beskar7MachineReconciler {
		return &Beskar7MachineReconciler{
			Client:    c,
			Scheme:    k8sClient.Scheme(),
			Log:       ctrl.Log.WithName("claim-record"),
			APIReader: k8sClient,
		}
	}

	heldHosts := func() []string {
		list := &infrav1.PhysicalHostList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(ns.Name))).To(Succeed())
		var names []string
		for i := range list.Items {
			if cr := list.Items[i].Spec.ConsumerRef; cr != nil && cr.Name == b7m.Name {
				names = append(names, list.Items[i].Name)
			}
		}
		return names
	}

	claim := func(r *Beskar7MachineReconciler) *infrav1.PhysicalHost {
		host, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, b7m, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(host).NotTo(BeNil())
		return host
	}

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "claim-record-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		createAvailableHost("host-a")
		createAvailableHost("host-b")
		b7m = &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "claim-record-machine",
				Namespace:  ns.Name,
				Finalizers: []string{Beskar7MachineFinalizer},
			},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspect.ipxe",
				TargetImageURL:     "http://boot-server/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())
		view = &staleHostView{namespace: ns.Name}
		view.catchUp()
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	Describe("on the claim side", func() {
		It("claims a second host without the record: the fixture reproduces the race", func() {
			claim(machineReconciler(view.client()))
			view.reversed = true

			claim(machineReconciler(view.client()))

			Expect(heldHosts()).To(ConsistOf("host-a", "host-b"))
		})

		It("finds the host it just claimed instead of claiming a second one", func() {
			r := machineReconciler(view.client())
			first := claim(r)
			view.reversed = true

			again := claim(r)

			Expect(again.Name).To(Equal(first.Name))
			Expect(heldHosts()).To(ConsistOf(first.Name))
		})

		It("finds a host whose claim landed with its answer lost", func() {
			r := machineReconciler(lostPatchAnswer(view.client()))
			_, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, b7m, nil)
			Expect(err).To(HaveOccurred())
			Expect(heldHosts()).To(ConsistOf("host-a"))
			view.reversed = true

			again := claim(r)

			Expect(again.Name).To(Equal("host-a"))
			Expect(heldHosts()).To(ConsistOf("host-a"))
		})

		It("drops a record whose live host now names another machine", func() {
			r := machineReconciler(view.client())
			first := claim(r)
			live := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(first), live)).To(Succeed())
			live.Spec.ConsumerRef.Name = "another-machine"
			live.Spec.ConsumerRef.UID = "another-uid"
			Expect(k8sClient.Update(ctx, live)).To(Succeed())
			view.reversed = true

			again := claim(r)

			Expect(again.Name).NotTo(Equal(first.Name))
			Expect(heldHosts()).To(ConsistOf(again.Name))
			recorded, _ := r.recentClaims.Load(b7m.UID)
			Expect(recorded).To(Equal(client.ObjectKeyFromObject(again)))
		})

		It("drops the record once the cache shows the claim", func() {
			r := machineReconciler(view.client())
			claim(r)
			view.catchUp()

			claim(r)

			_, recorded := r.recentClaims.Load(b7m.UID)
			Expect(recorded).To(BeFalse())
		})
	})

	Describe("for a pool machine whose recorded host is in another namespace", func() {
		const poolName = "claim-record-pool"

		var (
			poolMachine *infrav1.Beskar7Machine
			poolHost    *infrav1.PhysicalHost
		)

		BeforeEach(func() {
			hostNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "claim-record-hosts-"}}
			Expect(k8sClient.Create(ctx, hostNS)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, hostNS)).To(Succeed()) })
			poolMachine = &infrav1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "claim-record-pool-machine",
					Namespace: ns.Name,
				},
				Spec: infrav1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/inspect.ipxe",
					TargetImageURL:     "http://boot-server/kairos.tar.gz",
					TargetImageDigest:  bootTestDigest,
					PoolRef:            poolName,
				},
			}
			Expect(k8sClient.Create(ctx, poolMachine)).To(Succeed())
			poolHost = &infrav1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pool-host",
					Namespace: hostNS.Name,
				},
				Spec: infrav1.PhysicalHostSpec{
					RedfishConnection: infrav1.RedfishConnection{
						Address:              "https://192.168.100.1",
						CredentialsSecretRef: "irrelevant",
					},
					ConsumerRef: &corev1.ObjectReference{
						Kind:       "Beskar7Machine",
						APIVersion: InfrastructureAPIVersion,
						Name:       poolMachine.Name,
						Namespace:  poolMachine.Namespace,
					},
					ConsumerPool: poolName,
				},
			}
		})

		readRecorded := func() *infrav1.PhysicalHost {
			r := machineReconciler(view.client())
			r.HostPools = true
			r.recordClaim(poolMachine, poolHost)

			host, err := r.recordedHost(ctx, poolMachine, false)

			Expect(err).NotTo(HaveOccurred())
			_, recorded := r.recentClaims.Load(poolMachine.UID)
			Expect(recorded).To(Equal(host != nil))
			return host
		}

		It("finds a claim it can prove: the control for the spec below", func() {
			poolHost.Spec.ConsumerRef.UID = poolMachine.UID
			Expect(k8sClient.Create(ctx, poolHost)).To(Succeed())

			Expect(readRecorded()).NotTo(BeNil())
		})

		It("drops a forged claim that names the machine without its UID", func() {
			poolHost.Spec.ConsumerRef.UID = "forged-uid"
			Expect(k8sClient.Create(ctx, poolHost)).To(Succeed())

			Expect(readRecorded()).To(BeNil())
		})
	})

	Describe("on deletion", func() {
		BeforeEach(func() {
			// Skips the BMC power-off.
			b7m.Annotations = map[string]string{ForceReleaseAnnotation: "true"}
		})

		It("releases the host it just claimed before the finalizer comes off", func() {
			r := machineReconciler(view.client())
			claim(r)

			_, err := r.reconcileDelete(ctx, r.Log, b7m)

			Expect(err).NotTo(HaveOccurred())
			Expect(heldHosts()).To(BeEmpty())
			Expect(controllerutil.ContainsFinalizer(b7m, Beskar7MachineFinalizer)).To(BeFalse())
			_, recorded := r.recentClaims.Load(b7m.UID)
			Expect(recorded).To(BeFalse())
		})

		It("keeps the finalizer while the recorded host cannot be read", func() {
			r := machineReconciler(view.client())
			claim(r)
			r.APIReader = failingHostGets()

			_, err := r.reconcileDelete(ctx, r.Log, b7m)

			Expect(err).To(HaveOccurred())
			Expect(heldHosts()).To(ConsistOf("host-a"))
			Expect(controllerutil.ContainsFinalizer(b7m, Beskar7MachineFinalizer)).To(BeTrue())
		})
	})
})
