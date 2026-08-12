package litellm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Fake is an in-memory API implementation used by tests of the control plane.
// It keeps just enough state to make reconcile plans converge the way the real
// proxy does.
type Fake struct {
	Deployments []Deployment
	Keys        []Key

	// Calls records mutating operations in order, so tests can assert that a
	// second apply does nothing at all.
	Calls []string

	// FailOn makes the named operation return an error, to exercise error
	// paths (for example a proxy without /config/update).
	FailOn map[string]error
}

// NewFake returns an empty fake proxy.
func NewFake() *Fake {
	return &Fake{FailOn: map[string]error{}}
}

func (f *Fake) record(op string) error {
	if err, ok := f.FailOn[op]; ok {
		return err
	}
	f.Calls = append(f.Calls, op)
	return nil
}

// ListDeployments implements API.
func (f *Fake) ListDeployments(context.Context) ([]Deployment, error) {
	out := make([]Deployment, len(f.Deployments))
	copy(out, f.Deployments)
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out, nil
}

// CreateDeployment implements API.
func (f *Fake) CreateDeployment(_ context.Context, d Deployment) error {
	if err := f.record("create-deployment:" + d.ID()); err != nil {
		return err
	}
	f.Deployments = append(f.Deployments, d)
	return nil
}

// UpdateDeployment implements API.
func (f *Fake) UpdateDeployment(_ context.Context, d Deployment) error {
	if err := f.record("update-deployment:" + d.ID()); err != nil {
		return err
	}
	for i, existing := range f.Deployments {
		if existing.ID() == d.ID() {
			f.Deployments[i] = d
			return nil
		}
	}
	return fmt.Errorf("deployment %s not found", d.ID())
}

// DeleteDeployment implements API.
func (f *Fake) DeleteDeployment(_ context.Context, id string) error {
	if err := f.record("delete-deployment:" + id); err != nil {
		return err
	}
	kept := f.Deployments[:0]
	for _, existing := range f.Deployments {
		if existing.ID() != id {
			kept = append(kept, existing)
		}
	}
	f.Deployments = kept
	return nil
}

// ListKeys implements API.
func (f *Fake) ListKeys(context.Context) ([]Key, error) {
	out := make([]Key, len(f.Keys))
	copy(out, f.Keys)
	return out, nil
}

// GenerateKey implements API.
func (f *Fake) GenerateKey(_ context.Context, req GenerateKeyRequest) (GeneratedKey, error) {
	if err := f.record("generate-key:" + req.KeyAlias); err != nil {
		return GeneratedKey{}, err
	}
	budget := req.MaxBudget
	rpm, tpm := req.RPMLimit, req.TPMLimit
	f.Keys = append(f.Keys, Key{
		// The proxy stores the hash of the secret, never the secret. Two keys
		// of the same consumer therefore have different tokens, which is what
		// makes a rotation with a grace window addressable.
		Token:          fakeToken(req.Key),
		KeyName:        MaskSecret(req.Key),
		KeyAlias:       req.KeyAlias,
		Models:         req.Models,
		MaxBudget:      &budget,
		BudgetDuration: req.BudgetDuration,
		RPMLimit:       &rpm,
		TPMLimit:       &tpm,
		Metadata:       req.Metadata,
	})
	return GeneratedKey{Key: req.Key, KeyName: MaskSecret(req.Key), KeyAlias: req.KeyAlias}, nil
}

// UpdateKey implements API.
func (f *Fake) UpdateKey(_ context.Context, req UpdateKeyRequest) error {
	if err := f.record("update-key:" + req.Key); err != nil {
		return err
	}
	for i, key := range f.Keys {
		if key.Token != req.Key {
			continue
		}
		if req.Models != nil {
			f.Keys[i].Models = req.Models
		}
		if req.MaxBudget != nil {
			f.Keys[i].MaxBudget = req.MaxBudget
		}
		if req.BudgetDuration != "" {
			f.Keys[i].BudgetDuration = req.BudgetDuration
		}
		if req.RPMLimit != nil {
			f.Keys[i].RPMLimit = req.RPMLimit
		}
		if req.TPMLimit != nil {
			f.Keys[i].TPMLimit = req.TPMLimit
		}
		if req.KeyAlias != "" {
			f.Keys[i].KeyAlias = req.KeyAlias
		}
		if req.Duration != "" {
			f.Keys[i].Expires = "in " + req.Duration
		}
		if req.Metadata != nil {
			f.Keys[i].Metadata = req.Metadata
		}
		return nil
	}
	return fmt.Errorf("key %s not found", req.Key)
}

// DeleteKeys implements API.
func (f *Fake) DeleteKeys(_ context.Context, tokens []string) error {
	for _, token := range tokens {
		if err := f.record("delete-key:" + token); err != nil {
			return err
		}
	}
	kept := f.Keys[:0]
	for _, key := range f.Keys {
		drop := false
		for _, token := range tokens {
			if key.Token == token {
				drop = true
			}
		}
		if !drop {
			kept = append(kept, key)
		}
	}
	f.Keys = kept
	return nil
}

func fakeToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

var _ API = (*Fake)(nil)
