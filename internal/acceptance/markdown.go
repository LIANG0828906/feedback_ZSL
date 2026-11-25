package acceptance

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func writeMarkdown(path string, report *Report) error {
	var b strings.Builder
	status := "FAIL"
	if report.OK {
		status = "PASS"
	}
	b.WriteString("# 通道转换链路验证报告\n\n")
	fmt.Fprintf(&b, "- 结论: **%s**\n", status)
	fmt.Fprintf(&b, "- 开始: %s\n", report.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- 结束: %s\n", report.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- 输出目录: `%s`\n", report.OutputDir)
	fmt.Fprintf(&b, "- SQLite: `%s`\n\n", report.MainDB)

	b.WriteString("## 多版本计划\n\n")
	b.WriteString("| 版本 | 版本字段 | 升级补齐数 | 语义一致 | 差异 |\n|---:|---|---:|---|---|\n")
	for _, check := range report.LoaderChecks {
		fmt.Fprintf(&b, "| v%d | `%s` | %d | %t | `%s` |\n", check.Version, check.VersionField, check.TraceCount, check.SemanticOK, check.SemanticDiff)
	}
	b.WriteString("\n负向解析:\n\n")
	for _, check := range report.NegativeChecks {
		fmt.Fprintf(&b, "- %s: got `%s` field `%s`, ok=%t\n", check.Name, check.ActualCode, check.Field, check.OK)
	}
	fmt.Fprintf(&b, "\n升级痕迹: %d 条；降级损失报告: %d 份。\n\n", len(report.UpgradeTraces), len(report.LossReports))

	if report.MainFailure != nil {
		failure := report.MainFailure
		b.WriteString("## 异常隔离与续跑\n\n")
		fmt.Fprintf(&b, "- 注入分块: %d\n", failure.InjectedChunk)
		fmt.Fprintf(&b, "- 首轮失败数: %d\n", len(failure.FirstRunFailed))
		fmt.Fprintf(&b, "- 首轮已提交: `%v`\n", failure.FirstRunCommitted)
		fmt.Fprintf(&b, "- 重试提交: `%v`\n", failure.RetryCommitted)
		fmt.Fprintf(&b, "- 最终提交序列: `%v`\n", failure.FinalCommitSequence)
		fmt.Fprintf(&b, "- 隔离: %t；恢复: %t\n\n", failure.Isolated, failure.Recovered)
	}

	if report.ThreePaths != nil {
		three := report.ThreePaths
		b.WriteString("## 三路径一致性\n\n")
		fmt.Fprintf(&b, "- 图像: `%s`；分块数: %d；结论: %t\n", three.ImageID, three.ChunkCount, three.OK)
		for _, path := range three.Paths {
			fmt.Fprintf(&b, "- %s: ", path.Name)
			for _, state := range path.States {
				fmt.Fprintf(&b, "%s seq=%v; ", state.Name, state.CommitSequence)
			}
			b.WriteString("\n")
		}
		if three.FirstMismatch != nil {
			mismatch := three.FirstMismatch
			fmt.Fprintf(&b, "- 首个不一致: x=%d y=%d stage=%d field=%s expected=%d actual=%d\n", mismatch.X, mismatch.Y, mismatch.StageIndex, mismatch.Field, mismatch.Expected, mismatch.Actual)
		}
		if three.SequenceMismatch != "" {
			fmt.Fprintf(&b, "- 提交序列不一致: %s\n", three.SequenceMismatch)
		}
		b.WriteString("\n")
	}

	if report.IndependentVerify != nil {
		verify := report.IndependentVerify
		b.WriteString("## 独立验证工具\n\n")
		fmt.Fprintf(&b, "- OK: %t\n", verify.OK)
		fmt.Fprintf(&b, "- 真值指纹: %t；快照身份: %t\n", verify.TruthFingerprintOK, verify.SnapshotIdentityOK)
		fmt.Fprintf(&b, "- 采样点: %d；阶段采样: %d；扫描像素: %d；丢弃记录: %d\n", verify.CheckedSamples, verify.CheckedStagePoints, verify.CheckedPixels, verify.CheckedDroppedRecords)
		if verify.Violation != nil {
			v := verify.Violation
			fmt.Fprintf(&b, "- 首个违规: image=%s x=%d y=%d stage=%d field=%s message=%s\n", v.ImageID, v.X, v.Y, v.StageIndex, v.Field, v.Message)
		}
		b.WriteString("\n")
	}

	if report.ResourceProbe != nil {
		b.WriteString("## 资源边界\n\n")
		fmt.Fprintf(&b, "- 缓冲门阻塞等待: %dms；已记录排队: %t\n\n", report.ResourceProbe.BlockedWaitMS, report.ResourceProbe.Recorded)
	}

	if len(report.Errors) > 0 {
		b.WriteString("## 错误\n\n")
		for _, message := range report.Errors {
			fmt.Fprintf(&b, "- %s\n", message)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
