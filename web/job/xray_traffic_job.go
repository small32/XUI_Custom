package job

import (
	"sync"

	"x-ui/logger"
	"x-ui/web/service"
	"x-ui/xray"
)

type XrayTrafficJob struct {
	xrayService    service.XrayService
	inboundService service.InboundService

	// baseline 记录每个入站上一次已成功落库的累计 up/down。
	// GetXrayTraffic 现在读累计值且不再重置 xray 计数器，
	// 本 job 通过"当前累计 - 基线"得到增量，只有写库成功才推进基线。
	// 这样一旦写库失败，下一轮还能用新的累计减旧基线把缺口补回来，流量不丢失。
	mu       sync.Mutex
	baseline map[string]xray.Traffic
}

func NewXrayTrafficJob() *XrayTrafficJob {
	return &XrayTrafficJob{
		baseline: make(map[string]xray.Traffic),
	}
}

func (j *XrayTrafficJob) Run() {
	if !j.xrayService.IsXrayRunning() {
		j.mu.Lock()
		// A stopped/restarted Xray has a fresh stats counter. Do not subtract
		// the new process counters from the previous process baseline.
		j.baseline = make(map[string]xray.Traffic)
		j.mu.Unlock()
		return
	}
	cur, err := j.xrayService.GetXrayTraffic()
	if err != nil {
		logger.Warning("get xray traffic failed:", err)
		return
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	deltas := make([]*xray.Traffic, 0)
	for _, item := range cur {
		if !item.IsInbound {
			continue
		}
		last, ok := j.baseline[item.Tag]
		if !ok {
			// 首次见到该入站（或 xray 刚重启、计数归零），以当前累计为基线，不写增量，
			// 避免把 xray 的历史累计量误计。
			j.baseline[item.Tag] = *item
			continue
		}
		deltaUp := item.Up - last.Up
		deltaDown := item.Down - last.Down
		if deltaUp < 0 || deltaDown < 0 {
			// 计数被外部清零或 xray 重启：无法可靠计算增量，重置基线后跳过本轮。
			j.baseline[item.Tag] = *item
			continue
		}
		if deltaUp == 0 && deltaDown == 0 {
			continue
		}
		deltas = append(deltas, &xray.Traffic{IsInbound: true, Tag: item.Tag, Up: deltaUp, Down: deltaDown})
	}

	if len(deltas) == 0 {
		return
	}
	// 只有写库成功才推进基线；失败时基线保持不变，下一轮可重算缺口。
	newBaseline := make(map[string]xray.Traffic, len(cur))
	for _, item := range cur {
		if item.IsInbound {
			newBaseline[item.Tag] = *item
		}
	}
	if err := j.inboundService.AddTraffic(deltas); err != nil {
		logger.Warning("add traffic failed, will retry next round:", err)
		return
	}
	j.baseline = newBaseline
}
