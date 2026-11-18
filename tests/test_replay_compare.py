from orderlab_gateway.replay import compare_reports


def _report(rows=None, outcome="succeeded", code=None):
    return {
        "records": [
            {
                "query_id": "Q1",
                "dialect": "mssql-current",
                "repeat": 0,
                "outcome": outcome,
                "error_code": code,
                "columns": ["A"],
                "rows": rows or [],
                "result_digest": "abc" if rows else None,
            }
        ]
    }


def test_compare_detects_row_regression_after_restore():
    report = compare_reports(
        _report([[1]]),
        _report(outcome="rejected", code="E_INDEX_UNAVAILABLE"),
        _report([[2]]),
    )
    assert report["all_rows_consistent_after_restore"] is False
    assert report["disabled_index_rejections"] == ["Q1|mssql-current"]


def test_compare_accepts_identical_restored_rows():
    report = compare_reports(
        _report([[1]]),
        _report(outcome="rejected", code="E_INDEX_UNAVAILABLE"),
        _report([[1]]),
    )
    assert report["all_rows_consistent_after_restore"] is True
