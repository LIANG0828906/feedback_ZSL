"""Offline mastering renderer for the Ming-era opera stage soundscape task.

The package is fully deterministic: given the same cue sheet and integer seed
every generated WAV, every rendered master and every report field (timing
fields excepted) is byte-for-byte reproducible.
"""

__version__ = "1.0.0"

PART_BANGU = "板鼓"
PART_DALUO = "大锣"
PART_XIAOLUO = "小锣"
PART_BANGQIANG = "帮腔"
PART_YUEQIN = "月琴"
PART_BED = "底噪参考"

PERCUSSION_PARTS = (PART_BANGU, PART_DALUO, PART_XIAOLUO)
VOCAL_PARTS = (PART_BANGQIANG,)
YUEQIN_PARTS = (PART_YUEQIN,)
GROUP_PERCUSSION = "打击乐组"
GROUP_VOCAL = "帮腔组"
GROUP_YUEQIN = "月琴组"

ALL_PARTS = (
    PART_BANGU,
    PART_DALUO,
    PART_XIAOLUO,
    PART_BANGQIANG,
    PART_YUEQIN,
    PART_BED,
)

GROUP_OF_PART = {
    PART_BANGU: GROUP_PERCUSSION,
    PART_DALUO: GROUP_PERCUSSION,
    PART_XIAOLUO: GROUP_PERCUSSION,
    PART_BANGQIANG: GROUP_VOCAL,
    PART_YUEQIN: GROUP_YUEQIN,
}
