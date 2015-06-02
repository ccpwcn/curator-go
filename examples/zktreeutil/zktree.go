package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/flier/curator.go"
	"github.com/pmezard/go-difflib/difflib"
)

// The action type; any of create/delete/setvalue.
type ZkActionType int

const (
	NONE   ZkActionType = iota
	CREATE              // creates <zknode> recussively
	DELETE              // deletes <zknode> recursively
	VALUE               // sets <value> to <zknode>
)

type ZkAction struct {
	Type     ZkActionType // action of this instance
	Key      string       // ZK node key
	NewValue string       // value to be set, if action is setvalue
	OldValue string       // existing value of the ZK node key
}

type ZkActions []*ZkAction

type ZkActionHandler interface {
	Handle(action *ZkAction) error
}

type ZkActionPrinter struct {
	Out *os.File
}

func (p *ZkActionPrinter) Handle(action *ZkAction) error {
	var buf bytes.Buffer

	switch action.Type {
	case CREATE:
		fmt.Fprintf(&buf, "CREATE- key: %s\n", action.Key)
	case DELETE:
		fmt.Fprintf(&buf, "DELETE- key: %s\n", action.Key)
	case VALUE:
		fmt.Fprintf(&buf, "VALUE- key: %s value: %s", action.Key, action.NewValue)

		if len(action.OldValue) > 0 {
			fmt.Fprintf(&buf, " old: %s", action.OldValue)
		}

		fmt.Fprintln(&buf)
	}

	fmt.Print(buf.String())

	return nil
}

type ZkActionExecutor struct{}

func (e *ZkActionExecutor) Handle(action *ZkAction) error {
	return nil
}

type ZkActionInteractiveExecutor struct{}

func (e *ZkActionInteractiveExecutor) Handle(action *ZkAction) error {
	return nil
}

type ZkBaseNode struct {
	Path     string   `xml:"-"`
	Children []ZkNode `xml:"zknode"`
}

func (n *ZkBaseNode) Len() int {
	count := 1

	for _, child := range n.Children {
		count += child.Len()
	}

	return count
}

type ZkNode struct {
	ZkBaseNode

	XMLName xml.Name `xml:"zknode"`
	Name    string   `xml:"name,attr,omitempty"`
	Value   string   `xml:"value,attr,omitempty"`
	Ignore  *bool    `xml:"ignore,attr,omitempty"`
}

type ZkNodeContext struct {
	parent   *ZkNodeContext
	node     *ZkNode
	first    bool
	last     bool
	siblings []bool
}

func (c *ZkNodeContext) Path() string {
	var nodes []string

	for ctxt := c.parent; ctxt.node != nil; ctxt = ctxt.parent {
		nodes = append(nodes, ctxt.node.Name)
	}

	sort.Reverse(sort.StringSlice(nodes))

	return "/" + path.Join(nodes...)
}

type ZkNodeVisitFunc func(node *ZkNode, ctxt *ZkNodeContext) bool

func (n *ZkNode) Visit(visitor ZkNodeVisitFunc, ctxt *ZkNodeContext) {
	n.Path = path.Join(ctxt.Path(), n.Name)

	if visitor(n, ctxt) {
		for i, child := range n.Children {
			last := i == len(n.Children)-1

			child.Visit(visitor, &ZkNodeContext{
				parent:   ctxt,
				node:     &child,
				first:    i == 0,
				last:     last,
				siblings: append(ctxt.siblings, !last),
			})
		}
	}
}

type ZkRootNode struct {
	ZkBaseNode

	XMLName xml.Name `xml:"root"`
}

func (n *ZkRootNode) Visit(visitor ZkNodeVisitFunc, ctxt *ZkNodeContext) {
	for i, child := range n.Children {
		last := i == len(n.Children)-1

		child.Visit(visitor, &ZkNodeContext{
			parent:   ctxt,
			node:     &child,
			first:    i == 0,
			last:     last,
			siblings: append(ctxt.siblings, !last),
		})
	}
}

type ZkTree interface {
	Dump(depth int) (string, error)
}

type ZkBaseTree struct {
	getRoot func() (*ZkRootNode, error)
}

func (t *ZkBaseTree) Dump(depth int) (string, error) {
	if root, err := t.getRoot(); err != nil {
		return "", fmt.Errorf("fail to get root, %s", err)
	} else {
		var buf bytes.Buffer

		root.Visit(func(node *ZkNode, ctxt *ZkNodeContext) bool {
			level := len(ctxt.siblings)

			if len(node.Name) == 0 {
				return true // skip root
			}

			if depth > 0 && level > depth {
				return false // skip depth
			}

			for _, sibling := range ctxt.siblings[:level-1] {
				if sibling {
					fmt.Fprint(&buf, "|   ")
				} else {
					fmt.Fprint(&buf, "    ")
				}
			}

			if ctxt.first || ctxt.last {
				fmt.Fprintf(&buf, "+--[%s", node.Name)
			} else {
				fmt.Fprintf(&buf, "|--[%s", node.Name)
			}

			if len(node.Value) > 0 {
				fmt.Fprintf(&buf, " => %s", node.Value)
			}

			fmt.Fprintln(&buf, "]")

			return true
		}, &ZkNodeContext{first: true, last: true})

		return buf.String(), nil
	}
}

func (t *ZkBaseTree) Xml() ([]byte, error) {
	if root, err := t.getRoot(); err != nil {
		return nil, err
	} else if data, err := xml.MarshalIndent(root, "", "  "); err != nil {
		return nil, err
	} else {
		return []byte(fmt.Sprintf("%s%s\n", xml.Header, string(data))), nil
	}
}

type ZkLiveTree struct {
	ZkBaseTree

	client curator.CuratorFramework
}

func NewZkTree(hosts []string, base string) (*ZkLiveTree, error) {
	client := curator.NewClient(hosts[0], curator.NewRetryNTimes(3, time.Second))

	if err := client.Start(); err != nil {
		return nil, err
	}

	if len(base) > 0 {
		if base[0] == '/' {
			base = base[1:]
		}

		if len(base) > 0 {
			client = client.UsingNamespace(base)
		}
	}

	tree := &ZkLiveTree{client: client}

	tree.getRoot = tree.Root

	return tree, nil
}

// writes the in-memory ZK tree on to ZK server
func (t *ZkLiveTree) Merge(tree *ZkLoadedTree, force bool) error {
	if force {
		if len(t.client.Namespace()) > 0 {
			t.client.Delete().DeletingChildrenIfNeeded().ForPath("/")
		} else if children, err := t.client.GetChildren().ForPath("/"); err != nil {
			return err
		} else {
			for _, child := range children {
				if child != "zookeeper" {
					t.client.Delete().DeletingChildrenIfNeeded().ForPath(path.Join("/", child))
				} else {
					log.Printf("skip the `/%s` folder", child)
				}
			}
		}
	}

	tree.Root().Visit(func(node *ZkNode, ctxt *ZkNodeContext) bool {
		if strings.HasPrefix(node.Path, "/zookeeper") {
			return true
		}

		if err := t.mergeNode(node); err != nil {
			log.Fatalf("fail to merge node `%s`, %s", node.Path, err)

			return false
		}

		if node.Ignore != nil && *node.Ignore {
			log.Printf("ignore node @ `%s` updates", node.Path)

			return false
		} else {
			// Go deep to write the subtree rooted in the node, if not to be ignored

			return true
		}
	}, &ZkNodeContext{})

	return nil
}

func (t *ZkLiveTree) mergeNode(node *ZkNode) error {
	if stat, err := t.client.CheckExists().ForPath(node.Path); err != nil {
		return err
	} else if stat != nil {
		if stat, err := t.client.SetData().WithVersion(stat.Version).ForPathWithData(node.Path, []byte(node.Value)); err != nil {
			return err
		} else {
			log.Printf("merged node @ `%s` to version %d", node.Path, stat.Version)
		}
	} else {
		if path, err := t.client.Create().CreatingParentsIfNeeded().ForPathWithData(node.Path, []byte(node.Value)); err != nil {
			return err
		} else {
			log.Printf("created node @ `%s`", path)
		}
	}

	return nil
}

// returns a list of actions after taking a diff of in-memory ZK tree and live ZK tree.
func (t *ZkLiveTree) Diff(tree *ZkLoadedTree) error {
	tree.Root().Visit(func(node *ZkNode, ctxt *ZkNodeContext) bool {
		if strings.HasPrefix(node.Path, "/zookeeper") {
			return true
		}

		if err := t.diffNode(node); err != nil {
			log.Fatalf("fail to diff node `%s`, %s", node.Path, err)

			return false
		}

		return true
	}, &ZkNodeContext{})

	return nil
}

func (t *ZkLiveTree) diffNode(node *ZkNode) error {
	log.Printf("diff node @ `%s`", node.Path)

	diff := difflib.UnifiedDiff{
		FromFile: "a" + node.Path,
		ToFile:   "b" + node.Path,
		Context:  3,
	}

	if data, err := t.client.GetData().ForPath(node.Path); err == nil {
		diff.A = difflib.SplitLines(node.Value)
		diff.B = difflib.SplitLines(string(data))
	} else if err == curator.ErrNoNode {
		diff.A = difflib.SplitLines(node.Value)
	} else {
		return err
	}

	if err := difflib.WriteUnifiedDiff(os.Stdout, diff); err != nil {
		return err
	}

	return nil
}

func (t *ZkLiveTree) Node(znodePath string) (*ZkNode, error) {
	if data, err := t.client.GetData().ForPath(znodePath); err != nil {
		return nil, fmt.Errorf("fail to get data of node `%s`, %s", znodePath, err)
	} else if children, err := t.client.GetChildren().ForPath(znodePath); err != nil {
		return nil, fmt.Errorf("fail to get children of node `%s`, %s", znodePath, err)
	} else {
		var nodes []ZkNode

		for _, child := range children {
			if node, err := t.Node(path.Join(znodePath, child)); err != nil {
				return nil, err
			} else {
				nodes = append(nodes, *node)
			}
		}

		return &ZkNode{
			ZkBaseNode: ZkBaseNode{
				Path:     znodePath,
				Children: nodes,
			},
			Name:  path.Base(znodePath),
			Value: string(data),
		}, nil
	}
}

func (t *ZkLiveTree) Root() (*ZkRootNode, error) {
	if children, err := t.client.GetChildren().ForPath("/"); err != nil {
		return nil, fmt.Errorf("fail to get children of root, %s", err)
	} else {
		var nodes []ZkNode

		for _, child := range children {
			if node, err := t.Node(path.Join("/", child)); err != nil {
				return nil, err
			} else {
				nodes = append(nodes, *node)
			}
		}

		return &ZkRootNode{
			ZkBaseNode: ZkBaseNode{
				Path:     "/",
				Children: nodes,
			},
		}, nil
	}
}

type ZkLoadedTree struct {
	ZkBaseTree

	file *os.File
	root *ZkRootNode
}

func LoadZkTree(filename string) (*ZkLoadedTree, error) {
	if file, err := os.Open(filename); err != nil {
		return nil, fmt.Errorf("fail to open file `%s`, %s", filename, err)
	} else if data, err := ioutil.ReadFile(filename); err != nil {
		return nil, fmt.Errorf("fail to read file `%s`, %s", filename, err)
	} else {
		var root ZkRootNode

		if err := xml.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("fail to parse file `%s`, %s", filename, err)
		}

		return &ZkLoadedTree{
			ZkBaseTree: ZkBaseTree{
				getRoot: func() (*ZkRootNode, error) {
					return &root, nil
				},
			},
			file: file,
			root: &root,
		}, nil
	}
}

func (t *ZkLoadedTree) Root() *ZkRootNode {
	return t.root
}

func (t *ZkLoadedTree) String() (string, error) {
	return t.Dump(-1)
}