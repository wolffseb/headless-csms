package config

import "sort"

// The OCPI 2.3.0 enumerations we accept in config. Validating against them
// here means a typo is caught by `cpms config validate` rather than by the
// counterparty rejecting our Locations payload later.

// connectorStandards is OCPI 2.3.0 ConnectorType.
var connectorStandards = set(
	"CHADEMO", "CHAOJI",
	"DOMESTIC_A", "DOMESTIC_B", "DOMESTIC_C", "DOMESTIC_D", "DOMESTIC_E",
	"DOMESTIC_F", "DOMESTIC_G", "DOMESTIC_H", "DOMESTIC_I", "DOMESTIC_J",
	"DOMESTIC_K", "DOMESTIC_L", "DOMESTIC_M", "DOMESTIC_N", "DOMESTIC_O",
	"GBT_AC", "GBT_DC",
	"IEC_60309_2_single_16", "IEC_60309_2_three_16", "IEC_60309_2_three_32",
	"IEC_60309_2_three_64",
	"IEC_62196_T1", "IEC_62196_T1_COMBO", "IEC_62196_T2", "IEC_62196_T2_COMBO",
	"IEC_62196_T3A", "IEC_62196_T3C",
	"MCS",
	"NEMA_5_20", "NEMA_6_30", "NEMA_6_50", "NEMA_10_30", "NEMA_10_50",
	"NEMA_14_30", "NEMA_14_50",
	"PANTOGRAPH_BOTTOM_UP", "PANTOGRAPH_TOP_DOWN",
	"SAE_J3400", "TESLA_R", "TESLA_S",
)

// connectorFormats is OCPI 2.3.0 ConnectorFormat.
var connectorFormats = set("SOCKET", "CABLE")

// powerTypes is OCPI 2.3.0 PowerType.
var powerTypes = set("AC_1_PHASE", "AC_2_PHASE", "AC_2_PHASE_SPLIT", "AC_3_PHASE", "DC")

// ocppVersions are the protocol versions cpms knows how to speak.
var ocppVersions = set("1.6", "2.0.1")

func set(values ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(values))
	for _, v := range values {
		m[v] = struct{}{}
	}
	return m
}

// sorted returns a set's members in a stable order, for error messages.
func sorted(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
