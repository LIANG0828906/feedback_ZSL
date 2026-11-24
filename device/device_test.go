package device

import (
	"testing"

	"audiocal"
)

func setupDevice(t *testing.T) (*Server, *audiocal.CalibrationModel, []byte) {
	t.Helper()
	srv, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	sample := audiocal.GenerateSamples(20261124, 2)[0]
	model, err := audiocal.Fit(sample, audiocal.DefaultFitConfig())
	if err != nil {
		t.Fatal(err)
	}
	fw, err := audiocal.BuildFirmware(model)
	if err != nil {
		t.Fatal(err)
	}
	return srv, model, fw.Encode()
}

func TestEchoRoundTrip(t *testing.T) {
	srv, model, blob := setupDevice(t)
	client := NewClient(srv.Addr())
	resp, err := client.Upload(blob, model.Grid, model.SampleRate, 0.01, FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || !resp.ChecksumOK {
		t.Fatal("设备未确认校验通过")
	}
	for chID, applied := range resp.AppliedDB {
		rep, err := audiocal.CompareEcho(model, chID, applied, 0.01)
		if err != nil || !rep.Pass {
			t.Fatalf("通道 %d 回显超差: %v", chID, err)
		}
	}
}

func TestFaultInjections(t *testing.T) {
	srv, model, blob := setupDevice(t)
	client := NewClient(srv.Addr())
	cases := []struct {
		fault Fault
		code  audiocal.ErrorCode
	}{
		{FaultVersion, audiocal.CodeFW},
		{FaultWindow, audiocal.CodeWin},
		{FaultMap, audiocal.CodeMap},
		{FaultSegment, audiocal.CodeSeg},
	}
	for _, tc := range cases {
		_, err := client.Upload(blob, model.Grid, model.SampleRate, 0.01, tc.fault)
		be, ok := audiocal.AsBizError(err)
		if !ok || be.Code != tc.code {
			t.Fatalf("故障 %v 期望 %s，得到 %v", tc.fault, tc.code, err)
		}
	}
}

func TestEchoDriftTriggersEEcho(t *testing.T) {
	srv, model, blob := setupDevice(t)
	client := NewClient(srv.Addr())
	resp, err := client.Upload(blob, model.Grid, model.SampleRate, 0.01, FaultEchoDrift)
	if err != nil {
		t.Fatal(err)
	}
	hit := false
	for chID, applied := range resp.AppliedDB {
		_, err := audiocal.CompareEcho(model, chID, applied, 0.01)
		be, ok := audiocal.AsBizError(err)
		if ok && be.Code == audiocal.CodeEcho {
			if be.Detail["local_db"] == nil || be.Detail["echo_db"] == nil {
				t.Fatal("E_ECHO 未给出两侧取值")
			}
			hit = true
			break
		}
	}
	if !hit {
		t.Fatal("回显漂移未触发 E_ECHO")
	}
}

func TestRejectBadFirmwareMagic(t *testing.T) {
	srv, model, blob := setupDevice(t)
	client := NewClient(srv.Addr())
	bad := append([]byte(nil), blob...)
	copy(bad[:8], "XXXXXXXX")
	_, err := client.Upload(bad, model.Grid, model.SampleRate, 0.01, FaultNone)
	be, ok := audiocal.AsBizError(err)
	if !ok || be.Code != audiocal.CodeFW {
		t.Fatalf("坏魔数期望 E_FW，得到 %v", err)
	}
}
