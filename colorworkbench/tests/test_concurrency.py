"""业务要求 8：并发与资源边界、并行/串行一致、单批失败隔离。"""

import os
import unittest

import numpy as np

from cwb.engine import Workbench
from cwb.errors import CwbError, E_BATCH, E_DECODE
from cwb.executor import BoundedBuffer
from cwb.model import INTENT_RELATIVE

SAMPLES = os.path.join(os.path.dirname(__file__), "..", "samples")
IMG = os.path.join(SAMPLES, "images", "tile_48x32_8_rgba.cimg")
SRC = os.path.join(SAMPLES, "profiles", "src_v2.json")
DST = os.path.join(SAMPLES, "profiles", "dst_v2.json")


class ConcurrencyTests(unittest.TestCase):
    def test_parallel_and_serial_identical_output_and_sequence(self):
        results = {}
        sequences = {}
        for mode in (True, False):
            wb = Workbench(SRC, DST, INTENT_RELATIVE, output_depth=8,
                           batch_rows=8, max_workers=4,
                           pixel_buffer_cap=2048)
            wb.load_image(IMG, 8)
            results[mode] = wb.run_full(parallel=mode)
            sequences[mode] = list(wb.executor.commit_sequence)
        np.testing.assert_array_equal(results[True], results[False])
        self.assertEqual(sequences[True], sequences[False])
        # 批序稳定：按编号升序
        self.assertEqual(sequences[True], sorted(sequences[True]))

    def test_bounded_buffer_blocks_never_drops_batches(self):
        buf = BoundedBuffer(64)
        acquired = []
        import threading

        def worker(n):
            buf.acquire(n)
            acquired.append(n)
            buf.release(n)

        threads = [threading.Thread(target=worker, args=(64,))
                   for _ in range(6)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=10)
        self.assertEqual(len(acquired), 6)
        self.assertLessEqual(buf.peak_usage, 64)

    def test_single_batch_failure_does_not_pollute_others(self):
        wb = Workbench(SRC, DST, INTENT_RELATIVE, output_depth=8,
                       batch_rows=8, max_workers=3)
        wb.load_image(IMG, 8)
        original_run_batch = wb.pipeline.run_batch

        def flaky_run_batch(batch_id, raw_batch, reuse=True):
            if batch_id == 0:
                # 模拟单批内部异常：标记失败并抛出，不触碰其他批
                batch = wb.pipeline.batches[batch_id]
                batch.status = "failed"
                batch.error = "E_BATCH"
                raise RuntimeError("forced batch failure")
            return original_run_batch(batch_id, raw_batch, reuse)

        wb.pipeline.run_batch = flaky_run_batch
        wb.executor.run(wb.image, parallel=True)
        self.assertIn(0, wb.executor.failed_batches)
        self.assertEqual(wb.pipeline.batches[0].status, "failed")
        # 其他批仍然成功，结果不被污染
        good = [b.batch_id for b in wb.pipeline.batches[1:]]
        self.assertTrue(all(wb.pipeline.batches[i].status == "done"
                            for i in good))
        # 重试失败批后整体成功
        wb.pipeline.run_batch = original_run_batch
        out = wb.retry_failed(parallel=True)
        self.assertEqual(wb.pipeline.batches[0].status, "done")
        self.assertEqual(out.shape, wb.image.shape)

    def test_corrupt_image_returns_E_DECODE(self):
        for name in ("truncated", "badmagic"):
            wb = Workbench(SRC, DST, INTENT_RELATIVE, output_depth=8)
            with self.assertRaises(CwbError) as ctx:
                wb.load_image(os.path.join(
                    SAMPLES, "corrupt", name + ".cimg"))
            self.assertEqual(ctx.exception.code, E_DECODE)


if __name__ == "__main__":
    unittest.main()
