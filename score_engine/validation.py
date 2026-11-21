from __future__ import annotations

from fractions import Fraction

from .errors import score_invalid
from .model import ARTICULATIONS, Score


def validate_score(score: Score) -> None:
    if score.grid_resolution <= 0:
        raise score_invalid("grid_resolution 必须大于零", note_id="-", field="grid_resolution")
    if score.start_offset < 0:
        raise score_invalid("start_offset 不得为负", note_id="-", field="start_offset")
    if not score.time_signatures:
        raise score_invalid("至少需要一个拍号", note_id="-", field="time_signatures")
    if score.time_signatures[0].start != 0:
        raise score_invalid("首个拍号必须从第 0 拍开始", note_id="-", field="time_signatures[0].start")
    previous_start: Fraction | None = None
    for index, signature in enumerate(score.time_signatures):
        if previous_start is not None and signature.start <= previous_start:
            raise score_invalid(
                "拍号变化必须严格递增",
                note_id="-",
                field=f"time_signatures[{index}].start",
            )
        if signature.start % score.grid_resolution != 0:
            raise score_invalid(
                "拍号变化起点未落在统一节拍网格上",
                note_id="-",
                field=f"time_signatures[{index}].start",
            )
        if signature.numerator <= 0 or signature.denominator <= 0:
            raise score_invalid(
                "拍号必须为正数",
                note_id="-",
                field=f"time_signatures[{index}]",
            )
        previous_start = signature.start
    if not score.tempo_segments:
        raise score_invalid("至少需要一个速度段", note_id="-", field="tempo_segments")
    if score.tempo_segments[0].start != 0:
        raise score_invalid("首个速度段必须从第 0 拍开始", note_id="-", field="tempo_segments[0].start")
    previous_end: Fraction | None = None
    for index, segment in enumerate(score.tempo_segments):
        if segment.bpm <= 0:
            raise score_invalid("速度必须大于零", note_id="-", field=f"tempo_segments[{index}].bpm")
        if previous_end is not None and segment.start != previous_end:
            raise score_invalid(
                "速度段之间存在空隙或重叠",
                note_id="-",
                field=f"tempo_segments[{index}].start",
            )
        if segment.end is not None and segment.end <= segment.start:
            raise score_invalid(
                "速度段结束点必须大于起点",
                note_id="-",
                field=f"tempo_segments[{index}].end",
            )
        previous_end = segment.end
    if score.tempo_segments[-1].end is not None:
        raise score_invalid(
            "最后速度段必须保持开放结束",
            note_id="-",
            field=f"tempo_segments[{len(score.tempo_segments) - 1}].end",
        )
    part_ids: set[str] = set()
    note_ids: set[str] = set()
    for part_index, part in enumerate(score.parts):
        if not part.id or part.id in part_ids:
            raise score_invalid("声部编号为空或重复", note_id="-", field="id", part_id=part.id or None)
        part_ids.add(part.id)
        if not 0 <= part.min_pitch <= part.max_pitch <= 127:
            raise score_invalid(
                "音域声明必须满足 0 <= min <= max <= 127",
                note_id="-",
                field="pitch_range",
                part_id=part.id,
            )
        previous_note_start: Fraction | None = None
        for note in part.notes:
            if note.id in note_ids:
                raise score_invalid("音符编号重复", note_id=note.id, field="id", part_id=part.id)
            note_ids.add(note.id)
            if not 0 <= note.pitch <= 127:
                raise score_invalid("音高必须位于 0..127", note_id=note.id, field="pitch", part_id=part.id)
            if note.start < 0:
                raise score_invalid("音符起始拍不得为负", note_id=note.id, field="start", part_id=part.id)
            if note.duration <= 0:
                raise score_invalid("音符时值必须大于零", note_id=note.id, field="duration", part_id=part.id)
            if note.velocity < 1 or note.velocity > 127:
                raise score_invalid("力度必须位于 1..127", note_id=note.id, field="velocity", part_id=part.id)
            if note.articulation not in ARTICULATIONS:
                raise score_invalid("未知连奏标记", note_id=note.id, field="articulation", part_id=part.id)
            if note.start % score.grid_resolution != 0:
                raise score_invalid(
                    "音符起始拍未落在声明的节拍网格上，与拍号矛盾",
                    note_id=note.id,
                    field="start",
                    part_id=part.id,
                )
            if note.duration % score.grid_resolution != 0:
                raise score_invalid(
                    "音符时值不是声明的节拍网格的整数倍，与拍号矛盾",
                    note_id=note.id,
                    field="duration",
                    part_id=part.id,
                )
            if previous_note_start is not None and note.start < previous_note_start:
                raise score_invalid(
                    "声部内音符必须按起始拍排序",
                    note_id=note.id,
                    field="start",
                    part_id=part.id,
                )
            previous_note_start = note.start
        previous_pedal_end: Fraction | None = None
        for pedal_index, pedal in enumerate(part.pedals):
            if pedal.start < 0 or pedal.end <= pedal.start:
                raise score_invalid(
                    "踏板区间必须满足 0 <= start < end",
                    note_id="-",
                    field=f"pedals[{pedal_index}]",
                    part_id=part.id,
                )
            if not 0 <= pedal.value <= 1:
                raise score_invalid(
                    "踏板值必须位于 0..1",
                    note_id="-",
                    field=f"pedals[{pedal_index}].value",
                    part_id=part.id,
                )
            if previous_pedal_end is not None and pedal.start < previous_pedal_end:
                raise score_invalid(
                    "踏板区间不得重叠",
                    note_id="-",
                    field=f"pedals[{pedal_index}].start",
                    part_id=part.id,
                )
            previous_pedal_end = pedal.end
    expression_keys: set[tuple[Fraction, str | None]] = set()
    for index, anchor in enumerate(score.expression):
        if anchor.value < 0 or anchor.value > 1:
            raise score_invalid("表情锚点值必须位于 0..1", note_id="-", field=f"expression[{index}].value")
        if anchor.part_id is not None and anchor.part_id not in part_ids:
            raise score_invalid(
                "表情锚点引用了不存在声部",
                note_id="-",
                field=f"expression[{index}].part_id",
                part_id=anchor.part_id,
            )
        key = (anchor.start, anchor.part_id)
        if key in expression_keys:
            raise score_invalid(
                "同一作用域在同一拍不能有重复表情锚点",
                note_id="-",
                field=f"expression[{index}].start",
                part_id=anchor.part_id,
            )
        expression_keys.add(key)

