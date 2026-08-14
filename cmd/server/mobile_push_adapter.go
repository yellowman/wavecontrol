package main

import (
	"context"

	"github.com/yellowman/wavecontrol/internal/alerting"
	"github.com/yellowman/wavecontrol/internal/push"
)

type mobilePushAdapter struct {
	svc *push.Service
}

func (a mobilePushAdapter) EnqueueAlert(ctx context.Context, n alerting.MobileAlertNotification) error {
	if a.svc == nil {
		return nil
	}
	return a.svc.EnqueueAlert(ctx, push.AlertNotification{
		EventType:   n.EventType,
		AlertID:     n.AlertID,
		RuleID:      n.RuleID,
		RuleName:    n.RuleName,
		DeviceID:    n.DeviceID,
		DeviceIP:    n.DeviceIP,
		Hostname:    n.Hostname,
		SiteID:      n.SiteID,
		Metric:      n.Metric,
		Value:       n.Value,
		Threshold:   n.Threshold,
		Severity:    n.Severity,
		Message:     n.Message,
		TriggeredAt: n.TriggeredAt,
		ResolvedAt:  n.ResolvedAt,
	})
}
