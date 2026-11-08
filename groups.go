package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Groups let several raw category values (e.g. languages "en", "eng", "en-US")
// be browsed as one entity ("English") without editing any book. They are
// user-curated, so they live next to the library (never in metadata.db or the
// cache dir) and are written atomically with a backup of the previous version.

const groupsFileName = "calibrefast-groups.json"

type Group struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

type groupStore struct {
	mu       sync.RWMutex
	path     string
	data     map[string][]Group // category key ("language") -> groups
	readOnly error              // set when the file exists but couldn't be parsed
}

var groups = &groupStore{data: map[string][]Group{}}

var (
	errGroupsReadOnly = errors.New("groups are read-only")
	errGroupNameTaken = errors.New("another group already uses that name")
)

// loadGroups reads the store. A missing file is fine (no groups yet); an
// unreadable or malformed one makes the store read-only so it is never
// overwritten with less than it held.
func loadGroups(path string) *groupStore {
	s := &groupStore{path: path, data: map[string][]Group{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	if err == nil {
		err = json.Unmarshal(raw, &s.data)
	}
	if err != nil {
		s.readOnly = fmt.Errorf("%w: could not load %s (%v); fix or move the file and restart", errGroupsReadOnly, path, err)
		s.data = map[string][]Group{}
		log.Println("groups:", s.readOnly)
	}
	return s
}

func (s *groupStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), groupsFileName+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Keep the previous version around in case an edit was a mistake.
	if prev, err := os.ReadFile(s.path); err == nil {
		_ = os.WriteFile(s.path+".bak", prev, 0o644)
	}
	return os.Rename(tmp.Name(), s.path)
}

// findLocked returns the index of the group in cat whose name or a member
// matches v (case-insensitively, like Calibre's NOCASE columns). Name matches
// win over member matches.
func (s *groupStore) findLocked(cat, v string) int {
	gs := s.data[cat]
	for i, g := range gs {
		if strings.EqualFold(g.Name, v) {
			return i
		}
	}
	for i, g := range gs {
		for _, m := range g.Members {
			if strings.EqualFold(m, v) {
				return i
			}
		}
	}
	return -1
}

// resolve maps a URL value to the entity it stands for: its group if it is a
// group name or member, otherwise just itself.
func (s *groupStore) resolve(cat, v string) (name string, members []string, grouped bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i := s.findLocked(cat, v); i >= 0 {
		g := s.data[cat][i]
		return g.Name, append([]string(nil), g.Members...), true
	}
	return v, []string{v}, false
}

func copyGroups(gs []Group) []Group {
	out := make([]Group, len(gs))
	for i, g := range gs {
		out[i] = Group{Name: g.Name, Members: append([]string(nil), g.Members...)}
	}
	return out
}

// all returns a copy of the groups for a category.
func (s *groupStore) all(cat string) []Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyGroups(s.data[cat])
}

// snapshot returns a copy of every category's groups.
func (s *groupStore) snapshot() map[string][]Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]Group, len(s.data))
	for k, gs := range s.data {
		out[k] = copyGroups(gs)
	}
	return out
}

func cleanMembers(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, m := range in {
		m = strings.TrimSpace(m)
		k := strings.ToLower(m)
		if m == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, m)
	}
	return out
}

// set replaces the group that `key` resolves to (creating one if none) with g.
// Members are exclusive per category: they're taken out of any other group.
// No members, or a single member equal to the name, removes the group.
// Returns the stored group (zero value when removed).
func (s *groupStore) set(cat, key string, g Group) (Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readOnly != nil {
		return Group{}, s.readOnly
	}

	g.Name = strings.TrimSpace(g.Name)
	g.Members = cleanMembers(g.Members)
	if g.Name == "" && len(g.Members) > 0 {
		g.Name = g.Members[0]
	}

	gs := append([]Group(nil), s.data[cat]...)
	self := s.findLocked(cat, key)

	for i, other := range gs {
		if i != self && strings.EqualFold(other.Name, g.Name) {
			return Group{}, errGroupNameTaken
		}
	}

	trivial := len(g.Members) == 0 ||
		(len(g.Members) == 1 && strings.EqualFold(g.Members[0], g.Name))

	if self >= 0 {
		if trivial {
			gs = append(gs[:self], gs[self+1:]...)
		} else {
			gs[self] = g
		}
	} else if !trivial {
		gs = append(gs, g)
		self = len(gs) - 1
	}

	if !trivial {
		taken := map[string]bool{}
		for _, m := range g.Members {
			taken[strings.ToLower(m)] = true
		}
		kept := gs[:0]
		for _, other := range gs {
			if strings.EqualFold(other.Name, g.Name) {
				kept = append(kept, other)
				continue
			}
			ms := other.Members[:0:0]
			for _, m := range other.Members {
				if !taken[strings.ToLower(m)] {
					ms = append(ms, m)
				}
			}
			if len(ms) > 0 {
				other.Members = ms
				kept = append(kept, other)
			}
		}
		gs = kept
	}

	sort.Slice(gs, func(i, j int) bool { return strings.ToLower(gs[i].Name) < strings.ToLower(gs[j].Name) })
	prev := s.data[cat]
	s.data[cat] = gs
	if err := s.saveLocked(); err != nil {
		s.data[cat] = prev // don't report state we failed to persist
		return Group{}, err
	}
	if trivial {
		return Group{}, nil
	}
	return g, nil
}
