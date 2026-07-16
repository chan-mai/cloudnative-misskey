/*
Copyright (C) 2026 chan-mai

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MisskeyChannelSpec defines a fleet-level image channel that Misskey
// instances can follow via spec.imageFrom.
type MisskeyChannelSpec struct {
	// Image distributed to the instances following this channel.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Rollout staggers image changes across the fleet. Instances are hashed
	// into 100 stable buckets; each interval another batchPercent of buckets
	// switches to the new image. Omit for immediate rollout to everyone.
	// +optional
	Rollout *ChannelRollout `json:"rollout,omitempty"`

	// TrackImageDigest makes the operator resolve the image tag to its digest
	// and distribute image@digest, re-resolving periodically. Content pushed
	// under the same (mutable) tag then starts a new (staged) rollout across
	// the fleet. Anonymous registry access only — use digest-pinned instances
	// with imagePullSecrets for private registries.
	// +optional
	TrackImageDigest bool `json:"trackImageDigest,omitempty"`
}

// ChannelRollout configures the staged rollout pace.
type ChannelRollout struct {
	// BatchPercent of instances switched per interval.
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	BatchPercent int32 `json:"batchPercent,omitempty"`

	// Interval between batches.
	// +kubebuilder:default="1h"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`
}

// MisskeyChannelStatus is the observed rollout state.
type MisskeyChannelStatus struct {
	// Image is the current rollout target the instances converge to.
	// +optional
	Image string `json:"image,omitempty"`

	// PreviousImage is where not-yet-switched instances stay during a rollout.
	// +optional
	PreviousImage string `json:"previousImage,omitempty"`

	// ImageChangedAt is when the current rollout started.
	// +optional
	ImageChangedAt metav1.Time `json:"imageChangedAt,omitempty"`

	// Instances following this channel.
	// +optional
	Instances int32 `json:"instances,omitempty"`

	// UpdatedInstances already running the current image.
	// +optional
	UpdatedInstances int32 `json:"updatedInstances,omitempty"`

	// ObservedGeneration is the generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=mkch
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.image`
// +kubebuilder:printcolumn:name="Instances",type=integer,JSONPath=`.status.instances`
// +kubebuilder:printcolumn:name="Updated",type=integer,JSONPath=`.status.updatedInstances`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MisskeyChannel is a cluster-scoped image channel for staged fleet rollouts.
type MisskeyChannel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MisskeyChannelSpec   `json:"spec,omitempty"`
	Status MisskeyChannelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MisskeyChannelList contains a list of MisskeyChannel.
type MisskeyChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MisskeyChannel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MisskeyChannel{}, &MisskeyChannelList{})
}
