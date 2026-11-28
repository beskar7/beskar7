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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/beskar7/beskar7/api/v1beta2"
)

func (r *Beskar7MachineReconciler) recordClaim(b7machine *infrav1.Beskar7Machine, host *infrav1.PhysicalHost) {
	r.recentClaims.Store(b7machine.UID, client.ObjectKeyFromObject(host))
}

func (r *Beskar7MachineReconciler) forgetClaim(b7machine *infrav1.Beskar7Machine) {
	r.recentClaims.Delete(b7machine.UID)
}

// recordedHost reads the recorded host from the API server, since the cache may not have seen the claim yet.
func (r *Beskar7MachineReconciler) recordedHost(ctx context.Context, b7machine *infrav1.Beskar7Machine, forRelease bool) (*infrav1.PhysicalHost, error) {
	value, ok := r.recentClaims.Load(b7machine.UID)
	if !ok {
		return nil, nil
	}
	key := value.(client.ObjectKey)
	host := &infrav1.PhysicalHost{}
	err := r.liveReader().Get(ctx, key, host)
	if apierrors.IsNotFound(err) {
		r.forgetClaim(b7machine)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !r.holds(host, b7machine, forRelease) {
		r.forgetClaim(b7machine)
		return nil, nil
	}
	return host, nil
}

func (r *Beskar7MachineReconciler) holds(host *infrav1.PhysicalHost, b7machine *infrav1.Beskar7Machine, forRelease bool) bool {
	if host.Namespace == b7machine.Namespace {
		return r.claimedBy(host, b7machine)
	}
	return r.looksThroughPool(b7machine, forRelease) && provesPoolClaim(host, b7machine)
}

func (r *Beskar7MachineReconciler) looksThroughPool(b7machine *infrav1.Beskar7Machine, forRelease bool) bool {
	return b7machine.Spec.PoolRef != "" && (forRelease || r.HostPools)
}

func (r *Beskar7MachineReconciler) liveReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
