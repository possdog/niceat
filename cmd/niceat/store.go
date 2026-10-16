package main

import (
	bolt "go.etcd.io/bbolt"
)

// claimsDBName is the bbolt file backing the claims store, created in the
// working directory like the config and credential files.
const claimsDBName = "claims.bolt"

// claimsBucket is the single bbolt bucket holding label→DID bindings.
var claimsBucket = []byte("claims")

// keySep separates domain and label in the bbolt key.  Neither validated
// domains nor labels can contain NUL, so the composite key is unambiguous.
const keySep = "\x00"

// claims is the persistent record of which label is bound to which DID,
// namespaced by domain.  It exists so a handle can never be silently
// re-registered behind its owner's back: a second request for the same
// label is honored only as an idempotent repeat by the same DID, and
// refused for any other.
type claims struct {
	db *bolt.DB
}

// openClaims opens (creating if necessary) the claims database at path.
func openClaims(path string) (*claims, error) {
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(claimsBucket)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &claims{db: db}, nil
}

func (c *claims) close() error {
	return c.db.Close()
}

// claimKey builds the bbolt key for a label under a domain.  Namespacing by
// domain matters: "john" under wagg.ing and "john" under yeen.world are
// independent handles with independent owners.
func claimKey(domain, label string) []byte {
	return []byte(domain + keySep + label)
}

// tryClaim atomically records label→did under domain.  It returns
// created=true when the label was previously unclaimed.  When the label is
// already bound, created=false and existing is the bound DID — callers
// treat a matching DID as an idempotent repeat and anything else as a
// conflict.  The read-then-write happens inside a single bbolt write
// transaction (bbolt serializes writers), so concurrent requests cannot
// both win the same label.
func (c *claims) tryClaim(domain, label, did string) (created bool, existing string, err error) {
	key := claimKey(domain, label)
	err = c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(claimsBucket)
		if v := b.Get(key); v != nil {
			existing = string(v)
			return nil
		}
		if err := b.Put(key, []byte(did)); err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, existing, err
}

// release deletes a claim.  It is called only when provisioning failed
// after the claim was newly created, so a label is never left permanently
// bound to a handle that was never actually provisioned.  A pre-existing
// claim is never released this way — its owner keeps the label even if an
// idempotent re-provision attempt happens to fail.
func (c *claims) release(domain, label string) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(claimsBucket).Delete(claimKey(domain, label))
	})
}
