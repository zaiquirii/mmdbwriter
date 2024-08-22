package mmdbwriter

import (
	"fmt"
	"net"

	"github.com/zaiquirii/mmdbwriter/mmdbtype"
)

type recordType byte

const NoNode nodeId = -1
const (
	recordTypeEmpty recordType = iota
	recordTypeData
	recordTypeNode
	recordTypeAlias
	recordTypeFixedNode
	recordTypeReserved
)

type record struct {
	node       nodeId
	value      *dataMapValue
	recordType recordType
}

// each node contains two records.
type node struct {
	children [2]record
	nodeNum  int
}

type insertRecord struct {
	inserter func(value mmdbtype.DataType) (mmdbtype.DataType, error)

	dataMap      *dataMap
	insertedNode nodeId

	ip        net.IP
	prefixLen int

	recordType recordType
}

type nodeProvider func(nodeId) *node
type nodeInserter func(n node) nodeId

func (n *node) insert(iRec insertRecord, currentDepth int, np nodeProvider, ni nodeInserter) error {
	newDepth := currentDepth + 1
	// Check if we are inside the network already
	if newDepth > iRec.prefixLen {
		// Data already exists for the network so insert into all the children.
		// We will prune duplicate nodes when we finalize.
		err := n.children[0].insert(iRec, newDepth, np, ni)
		if err != nil {
			return err
		}
		return n.children[1].insert(iRec, newDepth, np, ni)
	}

	// We haven't reached the network yet.
	pos := bitAt(iRec.ip, currentDepth)
	r := &n.children[pos]
	return r.insert(iRec, newDepth, np, ni)
}

func (r *record) insert(
	iRec insertRecord,
	newDepth int,
	np nodeProvider,
	inserter nodeInserter,
) error {
	switch r.recordType {
	case recordTypeNode:
		err := np(r.node).insert(iRec, newDepth, np, inserter)
		if err != nil {
			return err
		}
		return r.maybeMergeChildren(iRec, np)
	case recordTypeFixedNode:
		return np(r.node).insert(iRec, newDepth, np, inserter)
	case recordTypeEmpty, recordTypeData:
		if newDepth >= iRec.prefixLen {
			r.node = iRec.insertedNode
			r.recordType = iRec.recordType
			if iRec.recordType == recordTypeData {
				var oldData mmdbtype.DataType
				if r.value != nil {
					oldData = r.value.data
				}
				newData, err := iRec.inserter(oldData)
				if err != nil {
					return err
				}
				if newData == nil {
					iRec.dataMap.remove(r.value)
					r.recordType = recordTypeEmpty
					r.value = nil
				} else if oldData == nil || !oldData.Equal(newData) {
					iRec.dataMap.remove(r.value)
					value, err := iRec.dataMap.store(newData)
					//nolint:revive //preexisting
					if err != nil {
						return err
					}
					r.value = value
				}
			} else {
				r.value = nil
			}
			return nil
		}

		// We are splitting this record so we create two duplicate child
		// records.
		r.node = inserter(node{children: [2]record{*r, *r}})
		r.value = nil
		r.recordType = recordTypeNode
		err := np(r.node).insert(iRec, newDepth, np, inserter)
		if err != nil {
			return err
		}
		return r.maybeMergeChildren(iRec, np)
	case recordTypeReserved:
		if iRec.prefixLen >= newDepth {
			return newReservedNetworkError(iRec.ip, newDepth, iRec.prefixLen)
		}
		// If we are inserting a network that contains a reserved network,
		// we silently remove the reserved network.
		return nil
	case recordTypeAlias:
		if iRec.prefixLen < newDepth {
			// Do nothing. We are inserting a network that contains an aliased
			// network. We silently ignore.
			return nil
		}
		// attempting to insert _into_ an aliased network
		return newAliasedNetworkError(iRec.ip, newDepth, iRec.prefixLen)
	default:
		return fmt.Errorf("inserting into record type %d is not implemented", r.recordType)
	}
}

func (r *record) maybeMergeChildren(iRec insertRecord, np nodeProvider) error {
	// Check to see if the children are the same and can be merged.
	child0 := np(r.node).children[0]
	child1 := np(r.node).children[1]
	if child0.recordType != child1.recordType {
		return nil
	}
	switch child0.recordType {
	// Nodes can't be merged
	case recordTypeFixedNode, recordTypeNode:
		return nil
	case recordTypeEmpty, recordTypeReserved:
		r.recordType = child0.recordType
		r.node = NoNode
		return nil
	case recordTypeData:
		if child0.value.key != child1.value.key {
			return nil
		}
		// Children have same data and can be merged
		r.recordType = recordTypeData
		r.value = child0.value
		iRec.dataMap.remove(child1.value)
		r.node = NoNode
		return nil
	default:
		return fmt.Errorf("merging record type %d is not implemented", child0.recordType)
	}
}

func (n *node) get(
	ip net.IP,
	depth int,
	np nodeProvider,
) (int, record) {
	r := n.children[bitAt(ip, depth)]

	depth++

	switch r.recordType {
	case recordTypeNode, recordTypeAlias, recordTypeFixedNode:
		return np(r.node).get(ip, depth, np)
	default:
		return depth, r
	}
}

// finalize  sets the node number for the node. It returns the current node
// count, including the subtree.
func (n *node) finalize(currentNum int, np nodeProvider) int {
	n.nodeNum = currentNum
	currentNum++

	for i := 0; i < 2; i++ {
		switch n.children[i].recordType {
		case recordTypeFixedNode,
			recordTypeNode:
			currentNum = np(n.children[i].node).finalize(currentNum, np)
		default:
		}
	}

	return currentNum
}

func bitAt(ip net.IP, depth int) byte {
	return (ip[depth/8] >> (7 - (depth % 8))) & 1
}
