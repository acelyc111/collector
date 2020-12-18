package aggregate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/XiaoMi/pegasus-go-client/idl/admin"
	"github.com/XiaoMi/pegasus-go-client/idl/base"
	"github.com/XiaoMi/pegasus-go-client/session"
	log "github.com/sirupsen/logrus"
	batchErr "k8s.io/apimachinery/pkg/util/errors"
)

// PerfClient manages sessions to all replica nodes.
type PerfClient struct {
	meta *session.MetaManager

	nodes map[string]*PerfSession
}

// GetPartitionStats retrieves all the partition stats from replica nodes.
// NOTE: Only the primaries are counted.
func (m *PerfClient) GetPartitionStats() ([]*PartitionStats, error) {
	m.updateNodes()

	partitions, err := m.preparePrimariesStats()
	if err != nil {
		return nil, err
	}

	nodeStats, err := m.GetNodeStats("@")
	if err != nil {
		return nil, err
	}

	for _, n := range nodeStats {
		for name, value := range n.Stats {
			perfCounter := decodePartitionPerfCounter(name, value)
			if perfCounter == nil {
				continue
			}
			if !aggregatable(perfCounter) {
				continue
			}
			part := partitions[perfCounter.gpid]
			if part == nil || part.Addr != n.Addr {
				// if this node is not the primary of this partition
				continue
			}

			part.Stats[perfCounter.name] = perfCounter.value
		}
	}

	var ret []*PartitionStats
	for _, part := range partitions {
		extendStats(&part.Stats)
		ret = append(ret, part)
	}
	return ret, nil
}

// getPrimaries returns mapping of [partition -> primary address]
func (m *PerfClient) getPrimaries() (map[base.Gpid]string, error) {
	tables, err := m.listTables()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	result, err := func() (map[base.Gpid]string, error) {
		result := make(map[base.Gpid]string)
		var mu sync.Mutex
		var funcs []func() error

		for _, tb := range tables {
			tb := tb
			funcs = append(funcs, func() (subErr error) {
				tableCfg, err := m.meta.QueryConfig(ctx, tb.AppName)
				mu.Lock()
				if err != nil {
					return err
				}
				for _, p := range tableCfg.Partitions {
					result[*p.Pid] = p.Primary.GetAddress()
				}
				mu.Unlock()
				return nil
			})
		}
		return result, batchErr.AggregateGoroutines(funcs...)
	}()

	return result, err
}

func (m *PerfClient) preparePrimariesStats() (map[base.Gpid]*PartitionStats, error) {
	primaries, err := m.getPrimaries()
	if err != nil {
		return nil, err
	}
	partitions := make(map[base.Gpid]*PartitionStats)
	for p, addr := range primaries {
		partitions[p] = &PartitionStats{
			Gpid:  p,
			Stats: make(map[string]float64),
			Addr:  addr,
		}
	}
	return partitions, nil
}

// NodeStat contains the stats of a replica node.
type NodeStat struct {
	// Address of the replica node.
	Addr string

	// perfCounter's name -> the value.
	Stats map[string]float64
}

// GetNodeStats retrieves all the stats matched with `filter` from replica nodes.
func (m *PerfClient) GetNodeStats(filter string) ([]*NodeStat, error) {
	m.updateNodes()

	results, err := func() ([]*NodeStat, error) {
		var results []*NodeStat
		var funcs []func() error
		var mu sync.Mutex

		for _, node := range m.nodes {
			node := node
			funcs = append(funcs, func() (subErr error) {
				stat := &NodeStat{
					Addr:  node.Address,
					Stats: make(map[string]float64),
				}
				perfCounters, err := node.GetPerfCounters(filter)
				fmt.Println(node.Address)
				mu.Lock()
				if err != nil {
					return err
				}
				for _, p := range perfCounters {
					stat.Stats[p.Name] = p.Value
				}
				results = append(results, stat)
				mu.Unlock()
				return nil
			})
		}
		return results, batchErr.AggregateGoroutines(funcs...)
	}()

	return results, err
}

func (m *PerfClient) listNodes() ([]*admin.NodeInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	resp, err := m.meta.ListNodes(ctx, &admin.ListNodesRequest{
		Status: admin.NodeStatus_NS_ALIVE,
	})
	if err != nil {
		return nil, err
	}
	return resp.Infos, nil
}

func (m *PerfClient) listTables() ([]*admin.AppInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	resp, err := m.meta.ListApps(ctx, &admin.ListAppsRequest{
		Status: admin.AppStatus_AS_AVAILABLE,
	})
	if err != nil {
		return nil, err
	}
	return resp.Infos, nil
}

// updateNodes
func (m *PerfClient) updateNodes() {
	nodeInfos, err := m.listNodes()
	if err != nil {
		log.Error("skip updating nodes due to list-nodes RPC failure: ", err)
		return
	}

	newNodes := make(map[string]*PerfSession)
	for _, n := range nodeInfos {
		addr := n.Address.GetAddress()
		node, found := m.nodes[addr]
		if !found {
			newNodes[addr] = NewPerfSession(addr)
		} else {
			newNodes[addr] = node
		}
	}
	for n, client := range m.nodes {
		// close the unused connections
		if _, found := newNodes[n]; !found {
			client.Close()
		}
	}
	m.nodes = newNodes
}

// NewPerfClient returns an instance of PerfClient.
func NewPerfClient(metaAddrs []string) *PerfClient {
	return &PerfClient{
		meta:  session.NewMetaManager(metaAddrs, session.NewNodeSession),
		nodes: make(map[string]*PerfSession),
	}
}
