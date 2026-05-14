package deviceallocation

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/types"
)

// DebugState is the top-level debug dump of the controller's internal device allocation state.
type DebugState struct {
	Pods   []DebugPod   `json:"pods"`
	Claims []DebugClaim `json:"claims"`
	Pools  []DebugPool  `json:"pools"`
}

// DebugPod represents a pod and the devices allocated to it.
type DebugPod struct {
	UID       types.UID `json:"uid"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Devices   []string  `json:"devices"`
}

// DebugClaim represents a ResourceClaim and the devices allocated to it.
type DebugClaim struct {
	Namespace  string          `json:"namespace"`
	Name       string          `json:"name"`
	Devices    []string        `json:"devices"`
	Releasable bool            `json:"releasable"`
	Consumers  []DebugConsumer `json:"consumers"`
}

// DebugPool represents a resource pool (driver + pool name) and its devices.
type DebugPool struct {
	Driver  string        `json:"driver"`
	Pool    string        `json:"pool"`
	Devices []DebugDevice `json:"devices"`
}

// DebugDevice represents a single device within a pool.
type DebugDevice struct {
	Device     string          `json:"device"`
	Claims     []string        `json:"claims"`
	Consumers  []DebugConsumer `json:"consumers"`
	Releasable bool            `json:"releasable"`
}

// DebugConsumer represents a pod consumer of a device or claim.
type DebugConsumer struct {
	UID       types.UID `json:"uid"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
}

// DebugDump returns a snapshot of the controller's internal state structured for debugging.
func (c *Controller) DebugDump(ctx context.Context) (*DebugState, error) {
	select {
	case <-c.hydrationCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	state := &DebugState{}

	// Build pods view: aggregate devices by pod UID
	type podInfo struct {
		uid       types.UID
		namespace string
		name      string
		devices   []string
	}
	podMap := map[types.UID]*podInfo{}
	for device, meta := range c.allocatedDevices {
		deviceStr := device.String()
		for _, consumer := range meta.Consumers {
			pi, ok := podMap[consumer.UID]
			if !ok {
				pi = &podInfo{uid: consumer.UID, namespace: consumer.Namespace, name: consumer.Name}
				podMap[consumer.UID] = pi
			}
			pi.devices = append(pi.devices, deviceStr)
		}
	}
	for _, pi := range podMap {
		sort.Strings(pi.devices)
		state.Pods = append(state.Pods, DebugPod{
			UID:       pi.uid,
			Namespace: pi.namespace,
			Name:      pi.name,
			Devices:   pi.devices,
		})
	}
	sort.Slice(state.Pods, func(i, j int) bool {
		if state.Pods[i].Namespace != state.Pods[j].Namespace {
			return state.Pods[i].Namespace < state.Pods[j].Namespace
		}
		return state.Pods[i].Name < state.Pods[j].Name
	})

	// Build claims view
	for nn, devices := range c.devicesPerClaim {
		meta := c.metadataPerClaim[nn]
		deviceStrs := make([]string, 0, len(devices))
		for d := range devices {
			deviceStrs = append(deviceStrs, d.String())
		}
		sort.Strings(deviceStrs)
		consumers := make([]DebugConsumer, len(meta.Consumers))
		for i, consumer := range meta.Consumers {
			consumers[i] = DebugConsumer{UID: consumer.UID, Namespace: consumer.Namespace, Name: consumer.Name}
		}
		state.Claims = append(state.Claims, DebugClaim{
			Namespace:  nn.Namespace,
			Name:       nn.Name,
			Devices:    deviceStrs,
			Releasable: meta.Releasable,
			Consumers:  consumers,
		})
	}
	sort.Slice(state.Claims, func(i, j int) bool {
		if state.Claims[i].Namespace != state.Claims[j].Namespace {
			return state.Claims[i].Namespace < state.Claims[j].Namespace
		}
		return state.Claims[i].Name < state.Claims[j].Name
	})

	// Build pools view: group devices by driver/pool
	type poolKey struct {
		driver, pool string
	}
	poolDevices := map[poolKey][]DebugDevice{}
	for device, meta := range c.allocatedDevices {
		pk := poolKey{driver: device.Driver.Value(), pool: device.Pool.Value()}
		claims := make([]string, 0)
		if claimSet, ok := c.claimsPerDevice[device]; ok {
			for nn := range claimSet {
				claims = append(claims, fmt.Sprintf("%s/%s", nn.Namespace, nn.Name))
			}
			sort.Strings(claims)
		}
		consumers := make([]DebugConsumer, len(meta.Consumers))
		for i, consumer := range meta.Consumers {
			consumers[i] = DebugConsumer{UID: consumer.UID, Namespace: consumer.Namespace, Name: consumer.Name}
		}
		poolDevices[pk] = append(poolDevices[pk], DebugDevice{
			Device:     device.Device.Value(),
			Claims:     claims,
			Consumers:  consumers,
			Releasable: meta.Releasable,
		})
	}
	for pk, devices := range poolDevices {
		sort.Slice(devices, func(i, j int) bool { return devices[i].Device < devices[j].Device })
		state.Pools = append(state.Pools, DebugPool{
			Driver:  pk.driver,
			Pool:    pk.pool,
			Devices: devices,
		})
	}
	sort.Slice(state.Pools, func(i, j int) bool {
		if state.Pools[i].Driver != state.Pools[j].Driver {
			return state.Pools[i].Driver < state.Pools[j].Driver
		}
		return state.Pools[i].Pool < state.Pools[j].Pool
	})

	return state, nil
}
