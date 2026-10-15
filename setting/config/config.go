package config

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

// ConfigManager 统一管理所有配置
type ConfigManager struct {
	configs map[string]interface{}
	mutex   sync.RWMutex
}

var GlobalConfig = NewConfigManager()

func NewConfigManager() *ConfigManager {
	return &ConfigManager{
		configs: make(map[string]interface{}),
	}
}

// Register 注册一个配置模块
func (cm *ConfigManager) Register(name string, config interface{}) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	cm.configs[name] = config
}

// Get 获取指定配置模块
func (cm *ConfigManager) Get(name string) interface{} {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()
	return cm.configs[name]
}

// LoadFromDB 从数据库加载配置
func (cm *ConfigManager) LoadFromDB(options map[string]string) error {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	for name, config := range cm.configs {
		prefix := name + "."
		configMap := make(map[string]string)

		// 收集属于此配置的所有选项
		for key, value := range options {
			if strings.HasPrefix(key, prefix) {
				configKey := strings.TrimPrefix(key, prefix)
				configMap[configKey] = value
			}
		}

		// 如果找到配置项，则更新配置
		if len(configMap) > 0 {
			if err := updateConfigFromMap(config, configMap); err != nil {
				common.SysError("failed to update config " + name + ": " + err.Error())
				continue
			}
		}
	}

	return nil
}

// SaveToDB 将配置保存到数据库
func (cm *ConfigManager) SaveToDB(updateFunc func(key, value string) error) error {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	for name, config := range cm.configs {
		configMap, err := configToMap(config)
		if err != nil {
			return err
		}

		for key, value := range configMap {
			dbKey := name + "." + key
			if err := updateFunc(dbKey, value); err != nil {
				return err
			}
		}
	}

	return nil
}

// 辅助函数：将配置对象转换为map
func configToMap(config interface{}) (map[string]string, error) {
	if locker, ok := config.(interface {
		RLock()
		RUnlock()
	}); ok {
		locker.RLock()
		defer locker.RUnlock()
	}
	result := make(map[string]string)

	val := reflect.ValueOf(config)
	if val.Kind() == reflect.Ptr {
		val = val.Elem()
	}

	if val.Kind() != reflect.Struct {
		return nil, nil
	}

	typ := val.Type()
	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		fieldType := typ.Field(i)

		// 跳过未导出字段
		if !fieldType.IsExported() {
			continue
		}

		// 获取json标签作为键名
		key := fieldType.Tag.Get("json")
		if key == "" || key == "-" {
			key = fieldType.Name
		}

		// 处理不同类型的字段
		var strValue string
		switch field.Kind() {
		case reflect.String:
			strValue = field.String()
		case reflect.Bool:
			strValue = strconv.FormatBool(field.Bool())
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			strValue = strconv.FormatInt(field.Int(), 10)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			strValue = strconv.FormatUint(field.Uint(), 10)
		case reflect.Float32, reflect.Float64:
			strValue = strconv.FormatFloat(field.Float(), 'f', -1, 64)
		case reflect.Ptr:
			// 处理指针类型：如果非 nil，序列化指向的值
			if !field.IsNil() {
				bytes, err := common.Marshal(field.Interface())
				if err != nil {
					return nil, err
				}
				strValue = string(bytes)
			} else {
				// nil 指针序列化为 "null"
				strValue = "null"
			}
		case reflect.Map, reflect.Slice, reflect.Struct:
			// 复杂类型使用JSON序列化
			bytes, err := common.Marshal(field.Interface())
			if err != nil {
				return nil, err
			}
			strValue = string(bytes)
		default:
			// 跳过不支持的类型
			continue
		}

		result[key] = strValue
	}

	return result, nil
}

// 辅助函数：从map更新配置对象
func updateConfigFromMap(config interface{}, configMap map[string]string) error {
	if locker, ok := config.(sync.Locker); ok {
		locker.Lock()
		defer locker.Unlock()
	}
	val := reflect.ValueOf(config)
	if val.Kind() != reflect.Ptr || val.IsNil() || val.Elem().Kind() != reflect.Struct {
		return nil
	}

	// Validate on an independent copy: JSON decoding may partially mutate its
	// target before returning a type error. Applying only after validation also
	// preserves existing pointer identities (for example the shared RWMap maps).
	data, err := common.Marshal(config)
	if err != nil {
		return err
	}
	trial := reflect.New(val.Elem().Type()).Interface()
	if err := common.Unmarshal(data, trial); err != nil {
		return err
	}
	if err := applyConfigFromMap(trial, configMap); err != nil {
		return err
	}
	return applyConfigFromMap(config, configMap)
}

func applyConfigFromMap(config interface{}, configMap map[string]string) error {
	val := reflect.ValueOf(config)
	if val.Kind() != reflect.Ptr {
		return nil
	}
	val = val.Elem()

	if val.Kind() != reflect.Struct {
		return nil
	}

	typ := val.Type()
	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		fieldType := typ.Field(i)

		// 跳过未导出字段
		if !fieldType.IsExported() {
			continue
		}

		// 获取json标签作为键名
		key := fieldType.Tag.Get("json")
		if key == "" || key == "-" {
			key = fieldType.Name
		}

		// 检查map中是否有对应的值
		strValue, ok := configMap[key]
		if !ok {
			continue
		}

		// 根据字段类型设置值
		if !field.CanSet() {
			continue
		}

		switch field.Kind() {
		case reflect.String:
			field.SetString(strValue)
		case reflect.Bool:
			boolValue, err := strconv.ParseBool(strValue)
			if err != nil {
				return fmt.Errorf("invalid value for %s: expected a boolean", key)
			}
			field.SetBool(boolValue)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			intValue, err := strconv.ParseInt(strValue, 10, field.Type().Bits())
			if err != nil {
				// Accept integral float strings emitted by legacy settings clients.
				floatValue, floatErr := strconv.ParseFloat(strValue, 64)
				if floatErr != nil || math.IsNaN(floatValue) || math.IsInf(floatValue, 0) || math.Trunc(floatValue) != floatValue {
					return fmt.Errorf("invalid value for %s: expected an integer", key)
				}
				intValue, err = strconv.ParseInt(strconv.FormatFloat(floatValue, 'f', 0, 64), 10, field.Type().Bits())
				if err != nil {
					return fmt.Errorf("invalid value for %s: integer out of range", key)
				}
			}
			field.SetInt(intValue)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			uintValue, err := strconv.ParseUint(strValue, 10, field.Type().Bits())
			if err != nil {
				floatValue, floatErr := strconv.ParseFloat(strValue, 64)
				if floatErr != nil || math.IsNaN(floatValue) || math.IsInf(floatValue, 0) || floatValue < 0 || math.Trunc(floatValue) != floatValue {
					return fmt.Errorf("invalid value for %s: expected an unsigned integer", key)
				}
				uintValue, err = strconv.ParseUint(strconv.FormatFloat(floatValue, 'f', 0, 64), 10, field.Type().Bits())
				if err != nil {
					return fmt.Errorf("invalid value for %s: integer out of range", key)
				}
			}
			field.SetUint(uintValue)
		case reflect.Float32, reflect.Float64:
			floatValue, err := strconv.ParseFloat(strValue, field.Type().Bits())
			if err != nil || math.IsNaN(floatValue) || math.IsInf(floatValue, 0) {
				return fmt.Errorf("invalid value for %s: expected a finite number", key)
			}
			field.SetFloat(floatValue)
		case reflect.Ptr:
			// 处理指针类型
			if strValue == "null" {
				field.Set(reflect.Zero(field.Type()))
			} else {
				// 如果指针是 nil，需要先初始化
				if field.IsNil() {
					field.Set(reflect.New(field.Type().Elem()))
				}
				// 反序列化到指针指向的值
				err := common.Unmarshal([]byte(strValue), field.Interface())
				if err != nil {
					return fmt.Errorf("invalid JSON for %s: %w", key, err)
				}
			}
		case reflect.Map:
			// json.Unmarshal merges into existing maps (keeps old keys that are
			// absent from the new JSON). Allocate a fresh map so removed keys
			// are properly cleared.
			fresh := reflect.New(field.Type())
			if err := common.Unmarshal([]byte(strValue), fresh.Interface()); err != nil {
				return fmt.Errorf("invalid JSON for %s: %w", key, err)
			}
			field.Set(fresh.Elem())
		case reflect.Slice:
			// json.Unmarshal merges into existing slices element-wise: a shorter
			// incoming array reuses the old elements, so fields absent from the
			// new JSON inherit the old element's value at the same index. Config
			// slices are registered with compile-time defaults, so decoding in
			// place would mix stored entries with default ones. Allocate a fresh
			// slice so the stored value is the only source of truth.
			fresh := reflect.New(field.Type())
			if err := common.Unmarshal([]byte(strValue), fresh.Interface()); err != nil {
				return fmt.Errorf("invalid JSON for %s: %w", key, err)
			}
			field.Set(fresh.Elem())
		case reflect.Struct:
			// Structs are intentionally decoded in place: absent fields keep
			// their current value instead of being reset to the zero value.
			err := common.Unmarshal([]byte(strValue), field.Addr().Interface())
			if err != nil {
				return fmt.Errorf("invalid JSON for %s: %w", key, err)
			}
		}
	}

	return nil
}

// ConfigToMap 将配置对象转换为map（导出函数）
func ConfigToMap(config interface{}) (map[string]string, error) {
	return configToMap(config)
}

// UpdateConfigFromMap 从map更新配置对象（导出函数）
func UpdateConfigFromMap(config interface{}, configMap map[string]string) error {
	return updateConfigFromMap(config, configMap)
}

// ExportAllConfigs 导出所有已注册的配置为扁平结构
func (cm *ConfigManager) ExportAllConfigs() map[string]string {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	result := make(map[string]string)

	for name, cfg := range cm.configs {
		configMap, err := ConfigToMap(cfg)
		if err != nil {
			continue
		}

		// 使用 "模块名.配置项" 的格式添加到结果中
		for key, value := range configMap {
			result[name+"."+key] = value
		}
	}

	return result
}
