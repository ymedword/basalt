package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/ymedword/basalt/internal/flow"
)

func parseFlowConfig(section any, present bool, instanceRuns int) (flow.Config, error) {
	cfg := flow.DefaultConfig()
	if present {
		fields, ok := section.(map[string]any)
		if !ok {
			return cfg, errors.New("flows 配置必须是映射")
		}
		for _, value := range fields {
			if value == nil {
				return cfg, errors.New("flows 配置不接受空值")
			}
		}
		raw, err := json.Marshal(section)
		if err != nil {
			return cfg, errors.New("flows 配置类型非法")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return cfg, errors.New("flows 配置包含未知键或非法类型")
		}
	}
	if instanceRuns == 0 {
		instanceRuns = 32
	}
	// The existing server shutdown contract is 70s. The app also validates
	// against the concrete server grace when assembling its Worker lifecycle.
	if err := cfg.Validate(instanceRuns, 70*time.Second); err != nil {
		return cfg, err
	}
	return cfg, nil
}
