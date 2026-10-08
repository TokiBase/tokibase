package devicecert

import "github.com/tokibase/tokibase/kernel"

func markerOf(stubbed bool) kernel.ModuleMarker {
	return kernel.ModuleMarker{
		Name:        "devicecert",
		Collections: []string{"_device_certs", "_devicecert_state"},
		Envs: []string{"TOKI_DEVICECERT", "TOKI_DEVICECERT_LISTEN", "TOKI_DEVICECERT_MTLS",
			"TOKI_DEVICECERT_LEAF_DAYS", "TOKI_DEVICECERT_SANS"},
		Files:   []string{"devicecert_leaf.key", "devicecert_leaf.pem", "devicecert_ca.pem"},
		Stubbed: stubbed,
	}
}
