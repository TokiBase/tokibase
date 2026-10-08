//go:build !no_kiosk

package kiosk

import "crypto/ed25519"

type fakeNode string

func (f fakeNode) NodeID() string              { return string(f) }
func (f fakeNode) HubID() string               { return "hub" }
func (f fakeNode) HubPub() ed25519.PublicKey   { return nil }
func (f fakeNode) Cert() string                { return "" }
func (f fakeNode) Sign([]byte) ([]byte, error) { return nil, nil }
