package peerdirectory

import (
	"sort"
	"sync"

	"github.com/bruwbird/lcp/go-lcpd/internal/lcpwire"
)

type Peer struct {
	PeerID           string
	RemoteAddr       string
	Connected        bool
	CustomMsgEnabled bool
	ManifestSent     bool
	LCPReady         bool
	RemoteManifest   *lcpwire.Manifest
}

type LCPPeer struct {
	PeerID         string
	RemoteAddr     string
	RemoteManifest lcpwire.Manifest
}

type Directory struct {
	mu    sync.RWMutex
	peers map[string]*Peer
}

func New() *Directory {
	return &Directory{
		peers: make(map[string]*Peer),
	}
}

func (d *Directory) UpsertPeer(peerID string, remoteAddr string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	p, ok := d.peers[peerID]
	if !ok {
		p = &Peer{PeerID: peerID}
		d.peers[peerID] = p
	}
	p.RemoteAddr = remoteAddr
}

func (d *Directory) MarkConnected(peerID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	p, ok := d.peers[peerID]
	if !ok {
		p = &Peer{PeerID: peerID}
		d.peers[peerID] = p
	}
	p.Connected = true
	d.updateLCPReadyLocked(p)
}

func (d *Directory) MarkDisconnected(peerID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	p, ok := d.peers[peerID]
	if !ok {
		p = &Peer{PeerID: peerID}
		d.peers[peerID] = p
	}
	p.Connected = false
	p.CustomMsgEnabled = false
	p.ManifestSent = false
	p.LCPReady = false
	p.RemoteManifest = nil
}

func (d *Directory) MarkCustomMsgEnabled(peerID string, enabled bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	p, ok := d.peers[peerID]
	if !ok {
		p = &Peer{PeerID: peerID}
		d.peers[peerID] = p
	}
	p.CustomMsgEnabled = enabled
	d.updateLCPReadyLocked(p)
}

func (d *Directory) MarkManifestSent(peerID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	p, ok := d.peers[peerID]
	if !ok {
		p = &Peer{PeerID: peerID}
		d.peers[peerID] = p
	}
	p.ManifestSent = true
	d.updateLCPReadyLocked(p)
}

func (d *Directory) MarkLCPReady(peerID string, remoteManifest lcpwire.Manifest) {
	d.mu.Lock()
	defer d.mu.Unlock()

	p, ok := d.peers[peerID]
	if !ok {
		p = &Peer{PeerID: peerID}
		d.peers[peerID] = p
	}

	manifestCopy := remoteManifest
	p.RemoteManifest = &manifestCopy
	d.updateLCPReadyLocked(p)
}

func (d *Directory) ListLCPPeers() []LCPPeer {
	d.mu.RLock()
	defer d.mu.RUnlock()

	out := make([]LCPPeer, 0, len(d.peers))
	for _, p := range d.peers {
		if !p.Connected || !p.CustomMsgEnabled || !p.LCPReady || p.RemoteManifest == nil {
			continue
		}
		out = append(out, LCPPeer{
			PeerID:         p.PeerID,
			RemoteAddr:     p.RemoteAddr,
			RemoteManifest: *p.RemoteManifest,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].PeerID == out[j].PeerID {
			return out[i].RemoteAddr < out[j].RemoteAddr
		}
		return out[i].PeerID < out[j].PeerID
	})

	return out
}

func (d *Directory) ListConnectedPeerIDs() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	out := make([]string, 0, len(d.peers))
	for _, p := range d.peers {
		if p == nil || !p.Connected || p.PeerID == "" {
			continue
		}
		out = append(out, p.PeerID)
	}

	sort.Strings(out)
	return out
}

func (d *Directory) RemoteManifestFor(peerID string) (*lcpwire.Manifest, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	p, ok := d.peers[peerID]
	if !ok || p.RemoteManifest == nil {
		return nil, false
	}

	manifestCopy := *p.RemoteManifest
	return &manifestCopy, true
}

func (d *Directory) GetPeer(peerID string) (Peer, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	p, ok := d.peers[peerID]
	if !ok || p == nil {
		return Peer{}, false
	}

	out := *p
	if p.RemoteManifest != nil {
		manifestCopy := *p.RemoteManifest
		out.RemoteManifest = &manifestCopy
	}
	return out, true
}

func (d *Directory) updateLCPReadyLocked(p *Peer) {
	if p == nil {
		return
	}
	p.LCPReady = p.Connected && p.CustomMsgEnabled && p.ManifestSent && p.RemoteManifest != nil
}
