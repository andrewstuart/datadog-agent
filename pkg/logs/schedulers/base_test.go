// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package schedulers

import (
	"testing"

	logsConfig "github.com/DataDog/datadog-agent/pkg/logs/config"
	"github.com/DataDog/datadog-agent/pkg/logs/service"
	"github.com/stretchr/testify/require"
)

type testSched struct {
	started bool
	stopped bool
}

func (t *testSched) Start(mgr SourceManager) {
	t.started = true
}

func (t *testSched) Stop() {
	t.stopped = true
}

func TestSchedulers(t *testing.T) {
	sch := &testSched{}

	ss := NewSchedulers()
	ss.AddScheduler(sch)

	require.False(t, sch.started)
	require.False(t, sch.stopped)

	ss.Start(logsConfig.NewLogSources(), service.NewServices())

	require.True(t, sch.started)
	require.False(t, sch.stopped)

	ss.Stop()

	require.True(t, sch.started)
	require.True(t, sch.stopped)
}