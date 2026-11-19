import unittest

from dualwrite.logic import status_decision


class StatusDecisionTests(unittest.TestCase):
    def setUp(self):
        self.priorities = {"east": 20, "south": 10}

    def test_newer_version_wins(self):
        decision = status_decision(
            incoming_version=8,
            incoming_site="south",
            current_version=7,
            current_site="east",
            priorities=self.priorities,
        )
        self.assertTrue(decision.adopt)
        self.assertEqual(decision.basis, "incoming_row_version_newer")

    def test_older_version_is_stale(self):
        decision = status_decision(
            incoming_version=6,
            incoming_site="east",
            current_version=7,
            current_site="south",
            priorities=self.priorities,
        )
        self.assertFalse(decision.adopt)
        self.assertEqual(decision.basis, "incoming_row_version_older")

    def test_equal_version_uses_site_priority(self):
        decision = status_decision(
            incoming_version=7,
            incoming_site="east",
            current_version=7,
            current_site="south",
            priorities=self.priorities,
        )
        self.assertTrue(decision.adopt)
        self.assertEqual(decision.basis, "equal_version_site_priority")

    def test_equal_version_lower_priority_loses(self):
        decision = status_decision(
            incoming_version=7,
            incoming_site="south",
            current_version=7,
            current_site="east",
            priorities=self.priorities,
        )
        self.assertFalse(decision.adopt)
        self.assertEqual(decision.basis, "equal_version_lower_site_priority")


if __name__ == "__main__":
    unittest.main()

