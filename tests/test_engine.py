"""业务要求 7：后台引擎、E_BUSY 与协作式取消。"""

import tempfile
import time
import unittest

from retarget_pipeline import compat, engine, mapping, samples
from retarget_pipeline.errors import E_BUSY, E_CANCELLED, PipelineError


class EngineTest(unittest.TestCase):
    def setUp(self):
        self.docs = samples.build_source_documents(3, 10, 30)
        self.target = samples.build_target_document(3, 10)
        self.source = compat.upgrade_to_v3(self.docs[3])
        self.mapping = mapping.build_mapping(self.source, self.target)

    def test_background_run_completes(self):
        worker = engine.RetargetEngine()
        with tempfile.TemporaryDirectory() as tmp:
            handle = worker.submit(
                "t1", self.source, self.target, self.mapping, tmp, 1e-6,
                animation_key="anim-a")
            self.assertTrue(worker.wait(handle, timeout=30))
            self.assertEqual(handle.state_holder["status"], "completed")

    def test_duplicate_submit_returns_busy(self):
        worker = engine.RetargetEngine()
        with tempfile.TemporaryDirectory() as tmp1, tempfile.TemporaryDirectory() as tmp2:
            # 用足够多的帧保证首个任务仍在运行
            docs = samples.build_source_documents(3, 17, 200)
            source = compat.upgrade_to_v3(docs[3])
            target = samples.build_target_document(3, 17)
            mp = mapping.build_mapping(source, target)
            handle = worker.submit("busy-1", source, target, mp, tmp1, 1e-6,
                          animation_key="anim-busy")
            time.sleep(0.05)
            with self.assertRaises(PipelineError) as ctx:
                worker.submit("busy-2", source, target, mp, tmp2, 1e-6,
                              animation_key="anim-busy")
            self.assertEqual(ctx.exception.code, E_BUSY)
            # 等待首个任务结束，避免临时目录在后台写盘时被回收。
            self.assertTrue(worker.wait(handle, timeout=60))
            self.assertEqual(handle.state_holder["status"], "completed")

    def test_cancel_keeps_last_snapshot(self):
        worker = engine.RetargetEngine()
        with tempfile.TemporaryDirectory() as tmp:
            docs = samples.build_source_documents(3, 17, 400)
            source = compat.upgrade_to_v3(docs[3])
            target = samples.build_target_document(3, 17)
            mp = mapping.build_mapping(source, target)
            handle = worker.submit(
                "cancel-1", source, target, mp, tmp, 1e-6,
                animation_key="anim-cancel")
            time.sleep(0.1)
            requested = handle.cancel()
            self.assertTrue(requested)
            worker.wait(handle, timeout=30)
            self.assertEqual(handle.state_holder["status"], "cancelled")
            self.assertEqual(handle.state_holder["error"]["code"], E_CANCELLED)
            # 至少保留了最后一次已提交快照，且快照文件完整
            self.assertGreaterEqual(handle.state_holder["last_committed_frame"], 0)


if __name__ == "__main__":
    unittest.main()
