package ovsdbstore

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
)

func rowString(row ovsdb.Row, column string) (string, error) {
	value, exists := row[column]
	if !exists {
		return "", nil
	}
	result, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("column %s is not a string", column)
	}
	return result, nil
}

func rowBool(row ovsdb.Row, column string) (bool, error) {
	value, exists := row[column]
	if !exists {
		return false, nil
	}
	result, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("column %s is not a boolean", column)
	}
	return result, nil
}

func rowInt64(row ovsdb.Row, column string) (int64, error) {
	value, exists := row[column]
	if !exists {
		return 0, nil
	}
	switch number := value.(type) {
	case int:
		return int64(number), nil
	case int8:
		return int64(number), nil
	case int16:
		return int64(number), nil
	case int32:
		return int64(number), nil
	case int64:
		return number, nil
	case uint:
		if uint64(number) > math.MaxInt64 {
			break
		}
		return int64(number), nil
	case uint64:
		if number > math.MaxInt64 {
			break
		}
		return int64(number), nil
	case float64:
		if math.Trunc(number) == number && number >= math.MinInt64 && number <= math.MaxInt64 {
			return int64(number), nil
		}
	}
	return 0, fmt.Errorf("column %s is not an integer", column)
}

func rowUUID(row ovsdb.Row, column string) (string, error) {
	value, exists := row[column]
	if !exists {
		return "", fmt.Errorf("column %s is missing", column)
	}
	return valueUUID(value)
}

func valueUUID(value any) (string, error) {
	switch typed := value.(type) {
	case ovsdb.UUID:
		if typed.GoUUID == "" {
			return "", fmt.Errorf("UUID is empty")
		}
		return typed.GoUUID, nil
	case *ovsdb.UUID:
		if typed == nil || typed.GoUUID == "" {
			return "", fmt.Errorf("UUID is empty")
		}
		return typed.GoUUID, nil
	default:
		return "", fmt.Errorf("value is not a UUID")
	}
}

func rowReference(row ovsdb.Row, column string, optional bool) (string, error) {
	value, exists := row[column]
	if !exists {
		if optional {
			return "", nil
		}
		return "", fmt.Errorf("column %s is missing", column)
	}
	if !optional {
		uuid, err := valueUUID(value)
		if err != nil {
			return "", fmt.Errorf("column %s: %w", column, err)
		}
		return uuid, nil
	}
	values, err := valueSet(value)
	if err != nil {
		return "", fmt.Errorf("column %s: %w", column, err)
	}
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 {
		return "", fmt.Errorf("column %s contains %d optional references", column, len(values))
	}
	uuid, err := valueUUID(values[0])
	if err != nil {
		return "", fmt.Errorf("column %s: %w", column, err)
	}
	return uuid, nil
}

func rowSet(row ovsdb.Row, column string) ([]interface{}, error) {
	value, exists := row[column]
	if !exists {
		return []interface{}{}, nil
	}
	values, err := valueSet(value)
	if err != nil {
		return nil, fmt.Errorf("column %s: %w", column, err)
	}
	return values, nil
}

func valueSet(value any) ([]interface{}, error) {
	switch typed := value.(type) {
	case ovsdb.OvsSet:
		return append([]interface{}(nil), typed.GoSet...), nil
	case *ovsdb.OvsSet:
		if typed == nil {
			return nil, fmt.Errorf("set is nil")
		}
		return append([]interface{}(nil), typed.GoSet...), nil
	default:
		// RFC 7047 permits a singleton set to be encoded as its atom.
		return []interface{}{value}, nil
	}
}

func rowStringSet(row ovsdb.Row, column string) ([]string, error) {
	values, err := rowSet(row, column)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		item, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("column %s contains a non-string value", column)
		}
		if _, duplicate := seen[item]; duplicate {
			return nil, fmt.Errorf("column %s contains duplicate value %q", column, item)
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	sort.Strings(result)
	return result, nil
}

func rowMap(row ovsdb.Row, column string) (map[interface{}]interface{}, error) {
	value, exists := row[column]
	if !exists {
		return map[interface{}]interface{}{}, nil
	}
	switch typed := value.(type) {
	case ovsdb.OvsMap:
		result := make(map[interface{}]interface{}, len(typed.GoMap))
		for key, value := range typed.GoMap {
			result[key] = value
		}
		return result, nil
	case *ovsdb.OvsMap:
		if typed == nil {
			return nil, fmt.Errorf("column %s map is nil", column)
		}
		result := make(map[interface{}]interface{}, len(typed.GoMap))
		for key, value := range typed.GoMap {
			result[key] = value
		}
		return result, nil
	case map[interface{}]interface{}:
		return typed, nil
	case map[string]string:
		result := make(map[interface{}]interface{}, len(typed))
		for key, value := range typed {
			result[key] = value
		}
		return result, nil
	default:
		return nil, fmt.Errorf("column %s is not a map", column)
	}
}

func rowStringMap(row ovsdb.Row, column string) (map[string]string, error) {
	values, err := rowMap(row, column)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(values))
	for rawKey, rawValue := range values {
		key, keyOK := rawKey.(string)
		value, valueOK := rawValue.(string)
		if !keyOK || !valueOK {
			return nil, fmt.Errorf("column %s contains a non-string key or value", column)
		}
		result[key] = value
	}
	return result, nil
}

func rowTime(row ovsdb.Row, column string) (time.Time, error) {
	value, err := rowString(row, column)
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("column %s is not an RFC3339 timestamp: %w", column, err)
	}
	return parsed.UTC(), nil
}

func rowOptionalTime(row ovsdb.Row, column string) (*time.Time, error) {
	value, err := rowString(row, column)
	if err != nil {
		return nil, err
	}
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, fmt.Errorf("column %s is not an RFC3339 timestamp: %w", column, err)
	}
	parsed = parsed.UTC()
	return &parsed, nil
}
