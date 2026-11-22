"""Central version and tuning constants.

Two independent version identifiers drive snapshot invalidation:

``DOMAIN_MODEL_VERSION``
    Structure of the in-memory :class:`~scoreworkbench.model.Score`.
``CONVERSION_RULES_VERSION``
    Behaviour of the schema import/export rules.  Bumping either of them makes
    every previously persisted snapshot unusable.
"""

DOMAIN_MODEL_VERSION = "4.0"
CONVERSION_RULES_VERSION = "3"
SNAPSHOT_FORMAT_VERSION = "1"

SUPPORTED_SCHEMA_VERSIONS = ("1.0", "2.0", "3.0")

PPQ = 480

ARTICULATIONS = ("staccato", "legato", "tenuto", "accent")
EXPRESSION_MARKS = ("none", "cresc", "dim", "rit", "a_tempo", "fermata")
EVENT_KINDS = ("velocity", "legato", "pedal", "pitch_bend")

RESOURCE_LIMITS = {
    "max_open_files": 8,
    "max_event_buffer": 50_000,
}

TICKS_PER_BEAT = PPQ
MICROS_PER_SECOND = 1_000_000
MILLIS_PER_SECOND = 1_000

DEFAULT_VELOCITY = 0.75
