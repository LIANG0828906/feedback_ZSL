"""行宽约束下的确定性折行与多屏切分。"""
from __future__ import annotations

from .errors import E_PARA, PipelineError

# 闭口/点号类：不得出现在行首；开口类：不得出现在行尾。
CLOSE_PUNCT = set("，。！？；：、）】》”’…—·」』%")
OPEN_PUNCT = set("「『（【《“‘")


def _can_break(text, pos):
    # 在 text[pos] 之前断行是否合法（pos 为新行首字符下标）。
    if pos <= 0 or pos >= len(text):
        return True
    if text[pos] in CLOSE_PUNCT:
        return False
    if text[pos - 1] in OPEN_PUNCT:
        return False
    return True


# 代价 = sum((max_chars - 行宽)^2 * wrap_weight)，动态规划求最小值；
# 同代价时取断点更靠前的方案，保证同一文本与同一参数结果逐行一致，
# 与线程或遍历顺序无关。标点遵循避头尾规则，不会孤立成行。
def wrap_text(text: str, max_chars: int, wrap_weight: float) -> list:
    """最小代价折行，返回行字符串列表。"""
    if max_chars <= 0:
        raise PipelineError(E_PARA, "每行最大字符数必须为正整数",
                            field="max_chars_per_line")
    if wrap_weight < 0:
        raise PipelineError(E_PARA, "换行权重必须非负", field="wrap_weight")
    n = len(text)
    if n == 0:
        return []

    inf = float("inf")
    dp = [inf] * (n + 1)
    br = [-1] * (n + 1)
    dp[0] = 0.0
    for j in range(1, n + 1):
        lo = max(0, j - max_chars)
        for i in range(lo, j):
            if dp[i] == inf:
                continue
            if i > 0 and not _can_break(text, i):
                continue
            width = j - i
            cost = dp[i] + (max_chars - width) ** 2 * wrap_weight
            if cost < dp[j]:
                dp[j] = cost
                br[j] = i
    if br[n] == -1 and n > 0:
        # 极端情况（如超长标点串）退化为硬折行，仍完全确定。
        return _hard_wrap(text, max_chars)

    lines = []
    j = n
    while j > 0:
        i = br[j]
        lines.append(text[i:j])
        j = i
    lines.reverse()
    return lines


def _hard_wrap(text, max_chars):
    lines = []
    pos = 0
    n = len(text)
    while pos < n:
        end = min(n, pos + max_chars)
        if end < n and not _can_break(text, end):
            probe = end
            while probe > pos and not _can_break(text, probe):
                probe -= 1
            if probe > pos:
                end = probe
        lines.append(text[pos:end])
        pos = end
    return lines


def lines_per_screen(line_height: int, margin: int, screen_height: int) -> int:
    """由行高、留白与屏高推导单屏行数上限。"""
    if line_height <= 0:
        raise PipelineError(E_PARA, "行高必须为正整数", field="line_height")
    if margin < 0:
        raise PipelineError(E_PARA, "留白必须为非负整数", field="margin")
    if screen_height <= 0:
        raise PipelineError(E_PARA, "屏高必须为正整数", field="screen_height")
    usable = screen_height - 2 * margin
    count = usable // line_height
    if count < 1:
        raise PipelineError(E_PARA, "屏高扣除留白后不足一行",
                            field="screen_height")
    return int(count)


# 折行阶段已禁止闭口标点出现在行首，切屏边界不会制造孤立标点行。
def split_screens(lines: list, line_height: int, margin: int,
                  screen_height: int) -> list:
    """把折行结果切分为连续多屏，返回 [[行, ...], ...]。"""
    per = lines_per_screen(line_height, margin, screen_height)
    if not lines:
        return []
    return [lines[i:i + per] for i in range(0, len(lines), per)]


def flatten(screens: list) -> list:
    return [ln for screen in screens for ln in screen]


def line_char_counts(screens: list) -> list:
    return [len(ln) for ln in flatten(screens)]
