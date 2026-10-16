package app

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/juliosaraiva/patrol-cd/internal/cli/output"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Tree drawing prefixes (upstream tree.go).
const (
	firstElemPrefix = `├─`
	lastElemPrefix  = `└─`
	pipe            = `│ `
)

// treeIndex is the parent/child structure of resource nodes. Roots and children are sorted
// by group/kind/namespace/name so the output is deterministic (upstream iterates maps).
type treeIndex struct {
	byUID    map[string]apiclient.ResourceNode
	children map[string][]string
	roots    []string
}

func nodeKey(n apiclient.ResourceNode) string {
	return n.Group + "/" + n.Kind + "/" + n.Namespace + "/" + n.Name
}

// indexTree builds the index of nodes: a node's first parent reference decides where it
// hangs; nodes without parents (or whose parent is unknown) are roots.
func indexTree(nodes []apiclient.ResourceNode) *treeIndex {
	t := &treeIndex{byUID: map[string]apiclient.ResourceNode{}, children: map[string][]string{}}
	for i, n := range nodes {
		uid := n.UID
		if uid == "" {
			uid = fmt.Sprintf("#%d:%s", i, nodeKey(n))
			n.UID = uid
		}
		t.byUID[uid] = n
	}
	for uid, n := range t.byUID {
		if len(n.ParentRefs) > 0 {
			if _, ok := t.byUID[n.ParentRefs[0].UID]; ok && n.ParentRefs[0].UID != "" {
				t.children[n.ParentRefs[0].UID] = append(t.children[n.ParentRefs[0].UID], uid)
				continue
			}
		}
		t.roots = append(t.roots, uid)
	}
	sortUIDs := func(uids []string) {
		sort.Slice(uids, func(i, j int) bool { return nodeKey(t.byUID[uids[i]]) < nodeKey(t.byUID[uids[j]]) })
	}
	sortUIDs(t.roots)
	for _, c := range t.children {
		sortUIDs(c)
	}
	return t
}

// walk visits every node depth-first with its drawing prefix.
func (t *treeIndex) walk(fn func(prefix string, n apiclient.ResourceNode)) {
	var visit func(prefix, uid string, depth int)
	visit = func(prefix, uid string, depth int) {
		if depth > 64 {
			return
		}
		fn(printPrefix(prefix), t.byUID[uid])
		chs := t.children[uid]
		for i, c := range chs {
			p := prefix + firstElemPrefix
			if i == len(chs)-1 {
				p = prefix + lastElemPrefix
			}
			visit(p, c, depth+1)
		}
	}
	for _, r := range t.roots {
		visit("", r, 0)
	}
}

// printPrefix turns the accumulated prefix into the drawn one (upstream printPrefix).
func printPrefix(p string) string {
	if strings.HasSuffix(p, firstElemPrefix) {
		p = strings.Replace(p, firstElemPrefix, pipe, strings.Count(p, firstElemPrefix)-1)
	} else {
		p = strings.ReplaceAll(p, firstElemPrefix, pipe)
	}
	blank := strings.Repeat(" ", len([]rune(lastElemPrefix)))
	if strings.HasSuffix(p, lastElemPrefix) {
		p = strings.Replace(p, lastElemPrefix, blank, strings.Count(p, lastElemPrefix)-1)
	} else {
		p = strings.ReplaceAll(p, lastElemPrefix, blank)
	}
	return p
}

func nodeHealth(n apiclient.ResourceNode) (status, reason string) {
	if n.Health != nil {
		return string(n.Health.Status), n.Health.Message
	}
	return "", ""
}

func nodeAge(n apiclient.ResourceNode, now time.Time) string {
	if n.CreatedAt == nil {
		return "<unknown>"
	}
	return duration.HumanDuration(now.Sub(n.CreatedAt.Time))
}

// printAppTree prints `app get -o tree[=detailed]`: KIND/NAME with the sync status, health
// and message of matching resource states.
func printAppTree(w io.Writer, tree *apiclient.ApplicationTree, states []*resourceState, detailed bool, now time.Time) {
	byName := map[string]*resourceState{}
	for _, s := range states {
		byName[s.Kind+"/"+s.Name] = s
	}
	tw := output.NewTable(w)
	if detailed {
		tw.Row("KIND/NAME", "STATUS", "HEALTH", "AGE", "MESSAGE", "REASON")
	} else {
		tw.Row("KIND/NAME", "STATUS", "HEALTH", "MESSAGE")
	}
	indexTree(tree.Nodes).walk(func(prefix string, n apiclient.ResourceNode) {
		health, reason := nodeHealth(n)
		status, message := "", ""
		if s := byName[n.Kind+"/"+n.Name]; s != nil {
			status, health, message = s.Status, s.Health, s.Message
		}
		if detailed {
			tw.Row(prefix+n.Kind+"/"+n.Name, status, health, nodeAge(n, now), message, reason)
		} else {
			tw.Row(prefix+n.Kind+"/"+n.Name, status, health, message)
		}
	})
	_ = tw.Flush()
}

// printResourceList prints `app resources` (upstream printResources).
func printResourceList(w io.Writer, tree *apiclient.ApplicationTree, listAll, orphaned bool, format string, now time.Time) {
	tw := output.NewTable(w)
	showManaged := !orphaned || listAll
	showOrphaned := orphaned || listAll
	switch format {
	case outTreeDetailed, outTree:
		detailed := format == outTreeDetailed
		if detailed {
			tw.Row("GROUP", "KIND", "NAMESPACE", "NAME", "ORPHANED", "AGE", "HEALTH", "REASON")
		} else {
			tw.Row("GROUP", "KIND", "NAMESPACE", "NAME", "ORPHANED")
		}
		printNodes := func(nodes []apiclient.ResourceNode, orphanedLabel string) {
			indexTree(nodes).walk(func(prefix string, n apiclient.ResourceNode) {
				if detailed {
					health, reason := nodeHealth(n)
					tw.Row(prefix+n.Group, n.Kind, n.Namespace, n.Name, orphanedLabel, nodeAge(n, now), health, reason)
				} else {
					tw.Row(prefix+n.Group, n.Kind, n.Namespace, n.Name, orphanedLabel)
				}
			})
		}
		if showManaged {
			printNodes(tree.Nodes, "No")
		}
		if showOrphaned {
			printNodes(tree.OrphanedNodes, "Yes")
		}
	default:
		tw.Row("GROUP", "KIND", "NAMESPACE", "NAME", "ORPHANED")
		if showManaged {
			for _, n := range tree.Nodes {
				if len(n.ParentRefs) == 0 {
					tw.Row(n.Group, n.Kind, n.Namespace, n.Name, "No")
				}
			}
		}
		if showOrphaned {
			for _, n := range tree.OrphanedNodes {
				tw.Row(n.Group, n.Kind, n.Namespace, n.Name, "Yes")
			}
		}
	}
	_ = tw.Flush()
}
