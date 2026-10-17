// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"sort"
	"time"
)

// expiringVirtualSlots 快过期账号在新会话候选集中的虚拟实例权重。
// 3:1 是温和偏好，不是固定比例：账号组成变化会自然改变最终占比。
const expiringVirtualSlots = 3

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；有效快过期账号出现 expiringVirtualSlots 次。调用方继续按
// 原有序列表哈希，即可让新会话对快过期账号形成温和偏好。重复项按 UID 排序后
// 展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变既有方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		// 积分保底必须与 Pick/pickEarliestExpiry 同口径：会话首次分配直接走本列表，
		// 若不在这里拦，触底号仍会被新会话分到收费模型上，绕过 pick 的保护。
		if p.floorBlockedForRealmModel(e, model, realm, now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if e := p.byUID[uid]; expiringNow(e, now) {
				slots = expiringVirtualSlots
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// AvailableUIDsForModelRealm 返回模型口径可用（healthyForModel）的账号 UID：在该模型上被
// 6004 限流的账号不列入，而在**其他模型**被限流的账号照常列入（模型豁免）；realm 非空时
// 再仅保留 Realm()==realm 的账号，realm=="" 时不按域过滤。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.floorBlockedForRealmModel(e, model, realm, now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
