"""Shared numeric constants for the synth generators."""

# Standard-normal quantiles at p=0.95 and p=0.99. Hardcoded rather than
# computed via scipy.stats.norm.ppf so that generator determinism never
# depends on which numerical library provided the ppf.
Z95 = 1.6448536269514722
Z99 = 2.3263478740408408
