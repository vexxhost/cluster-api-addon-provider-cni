/*
Copyright 2026 VEXXHOST, Inc.

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

package catalog

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

var (
	// ErrUnknownVersion means a version's minor is not in the catalog.
	ErrUnknownVersion = errors.New("version is not in the catalog")
	// ErrDowngrade means the requested version is older than the running one.
	ErrDowngrade = errors.New("downgrades are not supported")
	// ErrNoUpgradePath means no allowed step leads towards the target.
	ErrNoUpgradePath = errors.New("no upgrade path")
	// ErrNoCompatibleVersion means no catalog version supports the Kubernetes versions.
	ErrNoCompatibleVersion = errors.New("no catalog version supports the Kubernetes version")
)

// Supports reports whether the catalog lists cni's minor as supporting every
// given Kubernetes version.
func (c *Catalog) Supports(cni *semver.Version, kubernetes ...*semver.Version) bool {
	m := c.minorOf(cni)
	if m == nil {
		return false
	}
	for _, k := range kubernetes {
		if k != nil && !m.kubernetes.Check(release(k)) {
			return false
		}
	}
	return true
}

// Known reports whether v's minor is in the catalog.
func (c *Catalog) Known(v *semver.Version) bool {
	return c.minorOf(v) != nil
}

// Target resolves the CNI version to run.
//
// An empty requested version means the newest version that supports every
// given Kubernetes version. "MAJOR.MINOR" means the latest catalog patch of
// that minor, and a full version pins that version. Observed is the running
// version, or nil for a fresh install; the target never goes below it.
func (c *Catalog) Target(requested string, observed *semver.Version, kubernetes ...*semver.Version) (*semver.Version, error) {
	if requested == "" {
		for i := len(c.Minors) - 1; i >= 0; i-- {
			m := &c.Minors[i]
			if c.Supports(m.version, kubernetes...) {
				return atLeast(m.version, observed), nil
			}
		}
		return nil, ErrNoCompatibleVersion
	}

	v, err := semver.NewVersion(requested)
	if err != nil {
		return nil, fmt.Errorf("invalid version %q: %w", requested, err)
	}
	m := c.minorOf(v)
	if m == nil {
		return nil, fmt.Errorf("%s: %w", requested, ErrUnknownVersion)
	}
	if strings.Count(strings.TrimPrefix(requested, "v"), ".") == 1 {
		v = m.version
		if observed != nil && sameMinor(observed, v) {
			v = atLeast(v, observed)
		}
	}
	if observed != nil && v.LessThan(observed) {
		return nil, fmt.Errorf("%s is older than the running %s: %w", c.Format(v), c.Format(observed), ErrDowngrade)
	}
	return v, nil
}

// NextHop returns the next version on the way from observed to target. A fresh
// install and an upgrade within one minor go straight to the target. Before
// leaving a minor, the CNI first moves to that minor's latest patch, as CNI
// upgrade guides recommend. Across minors every step follows the catalog's
// upgrade rule and lands on the latest patch of the furthest allowed minor.
// Kubernetes ranges are not checked here: only the target has to support the
// cluster.
func (c *Catalog) NextHop(observed, target *semver.Version) (*semver.Version, error) {
	if observed == nil || !observed.LessThan(target) || sameMinor(observed, target) {
		return target, nil
	}
	current := c.minorOf(observed)
	if current == nil {
		return nil, fmt.Errorf("%s: %w", c.Format(observed), ErrUnknownVersion)
	}
	if observed.LessThan(current.version) {
		return current.version, nil
	}
	for i := len(c.Minors) - 1; i >= 0; i-- {
		m := &c.Minors[i]
		if m.minor.GreaterThan(target) {
			continue
		}
		if !m.minor.GreaterThan(observed) {
			break
		}
		if c.canUpgrade(observed, m) {
			if sameMinor(m.minor, target) {
				return target, nil
			}
			return m.version, nil
		}
	}
	return nil, fmt.Errorf("from %s towards %s: %w", c.Format(observed), c.Format(target), ErrNoUpgradePath)
}

// Path returns every step from observed to target, ending with target.
func (c *Catalog) Path(observed, target *semver.Version) ([]*semver.Version, error) {
	var steps []*semver.Version
	for cur := observed; cur == nil || cur.LessThan(target); {
		next, err := c.NextHop(cur, target)
		if err != nil {
			return nil, err
		}
		steps = append(steps, next)
		cur = next
	}
	return steps, nil
}

func (c *Catalog) canUpgrade(from *semver.Version, to *Minor) bool {
	if to.upgradeFrom != nil {
		return to.upgradeFrom.Check(release(from))
	}
	return from.Major() == to.minor.Major() && to.minor.Minor()-from.Minor() <= c.MaxMinorSkip
}

func sameMinor(a, b *semver.Version) bool {
	return a.Major() == b.Major() && a.Minor() == b.Minor()
}

func atLeast(v, floor *semver.Version) *semver.Version {
	if floor != nil && v.LessThan(floor) {
		return floor
	}
	return v
}

// release drops prerelease and build metadata, so that a Kubernetes version
// such as v1.33.2+k3s1 matches the catalog's ranges.
func release(v *semver.Version) *semver.Version {
	return semver.New(v.Major(), v.Minor(), v.Patch(), "", "")
}
