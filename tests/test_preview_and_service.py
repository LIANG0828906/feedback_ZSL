from __future__ import annotations

import threading
from dataclasses import replace

import pytest

from npr_pipeline.errors import E_BUSY, E_CANCELLED, E_STALE_HANDLE, PipelineError
from npr_pipeline.pipeline import RenderPipeline
from npr_pipeline.preview import PreviewService, RenderService
from npr_pipeline.storage import Store


def test_preview_snapshot_is_version_atomic_and_stale_handle_fails(
    balanced_scene,
    balanced_params,
    tmp_path,
) -> None:
    store = Store(tmp_path / "work")
    first = RenderPipeline(store).render(balanced_scene, balanced_params)
    preview = PreviewService(store)
    old = preview.refresh()
    assert old is not None
    assert old.version == 1
    assert old.content_hash == first.version_hash
    assert all(batch.content_hash for batch in old.batches)
    assert {batch.layer for batch in old.batches} == {0, 1, 2}

    changed = replace(
        balanced_params,
        smear=replace(balanced_params.smear, width=balanced_params.smear.width + 0.6),
    )
    second = RenderPipeline(store).render(balanced_scene, changed)
    new = preview.refresh()
    assert new is not None
    assert new.version == 2
    assert new.content_hash == second.version_hash
    with pytest.raises(PipelineError) as caught:
        preview.resolve_handle(old)
    assert caught.value.code == E_STALE_HANDLE
    assert preview.resolve_handle(new) is new


def test_render_service_busy_and_cooperative_cancel(
    balanced_scene,
    balanced_params,
    tmp_path,
) -> None:
    store = Store(tmp_path / "work")
    first = RenderPipeline(store).render(balanced_scene, balanced_params)
    preview = PreviewService(store)
    old = preview.refresh()
    assert old is not None and old.version == first.version
    service = RenderService(preview)
    entered = threading.Event()
    release = threading.Event()

    def block_first_batch(index: int, batch_id: str) -> None:
        if index == 0:
            entered.set()
            assert release.wait(10.0)

    changed = replace(
        balanced_params,
        shadow=replace(
            balanced_params.shadow,
            spacing=balanced_params.shadow.spacing + 0.75,
        ),
    )
    task = service.submit(
        balanced_scene,
        changed,
        store,
        before_batch=block_first_batch,
    )
    assert entered.wait(10.0)
    with pytest.raises(PipelineError) as caught:
        service.submit(balanced_scene, changed, store)
    assert caught.value.code == E_BUSY
    assert service.cancel(balanced_scene.scene_id)
    release.set()
    assert task.wait(10.0)
    assert task.error is not None
    assert task.error.code == E_CANCELLED
    assert service.is_busy(balanced_scene.scene_id) is False
    current = store.current_manifest()
    assert current is not None and current["version"] == first.version
    still_old = preview.refresh()
    assert still_old is not None
    assert still_old.content_hash == old.content_hash
    assert preview.resolve_handle(still_old) is still_old

