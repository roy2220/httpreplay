package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
)

type curlCommand struct {
	URL     string
	Request string
	Header  *bytes.Buffer
	Data    *bytes.Buffer
}

func parseCurlCommand(args []string) (curlCommand, error) {
	var (
		curlCommand1           curlCommand
		contentTypeHeaderIsSet bool
		urlIsPresent           bool
	)
	i := 0
	n := len(args)
	popNextArg := func() (string, bool) {
		i++
		if i < n {
			return args[i], true
		}
		return "", false
	}
	for ; i < n; i++ {
		arg := args[i]
		if v, err, ok := getFlagValue(arg, "-X", "--request", popNextArg); ok {
			if err != nil {
				return curlCommand{}, err
			}
			curlCommand1.Request = v
			continue
		}
		if v, err, ok := getFlagValue(arg, "-H", "--header", popNextArg); ok {
			if err != nil {
				return curlCommand{}, err
			}
			key, _, ok := strings.Cut(v, ":")
			if !ok {
				return curlCommand{}, fmt.Errorf("invalid header: %v", v)
			}
			if strings.ToLower(key) == "content-type" {
				contentTypeHeaderIsSet = true
			}
			header := curlCommand1.Header
			if header == nil {
				header = new(bytes.Buffer)
				curlCommand1.Header = header
			} else {
				header.WriteString("\r\n")
			}
			header.WriteString(v)
			continue
		}
		if v, err, ok := getFlagValue(arg, "-d", "--data", popNextArg); ok {
			if err != nil {
				return curlCommand{}, err
			}
			data := curlCommand1.Data
			if data == nil {
				data = new(bytes.Buffer)
				curlCommand1.Data = data
			} else {
				data.WriteByte('&')
			}
			data.WriteString(v)
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return curlCommand{}, fmt.Errorf("unsupported flag: %v", arg)
		}
		if !urlIsPresent {
			curlCommand1.URL = arg
			urlIsPresent = true
		}
	}
	if !urlIsPresent {
		return curlCommand{}, fmt.Errorf("missing url")
	}
	if curlCommand1.URL == "" {
		return curlCommand{}, fmt.Errorf("empty url")
	}
	if curlCommand1.Data != nil {
		if curlCommand1.Request == "" {
			curlCommand1.Request = http.MethodPost
		}
		if !contentTypeHeaderIsSet {
			header := curlCommand1.Header
			if header == nil {
				header = new(bytes.Buffer)
				curlCommand1.Header = header
			} else {
				header.WriteString("\r\n")
			}
			header.WriteString("Content-Type: application/x-www-form-urlencoded")
		}
	}
	if curlCommand1.Request == "" {
		curlCommand1.Request = http.MethodGet
	}
	if curlCommand1.Header != nil {
		curlCommand1.Header.WriteString("\r\n\r\n")
	}
	return curlCommand1, nil
}

func getFlagValue(arg, flagName, longFlagName string, popNextArg func() (string, bool)) (string, error, bool) {
	longFlagMode := false
	v := strings.TrimPrefix(arg, flagName)
	if len(v) == len(arg) {
		longFlagMode = true
		v = strings.TrimPrefix(arg, longFlagName)
	}
	if len(v) == len(arg) {
		return "", nil, false
	}
	if v == "" {
		var ok bool
		v, ok = popNextArg()
		if !ok {
			return "", fmt.Errorf("missing flag value for %v/%v", flagName, longFlagName), true
		}
	} else if v[0] == '=' {
		v = v[1:]
	} else {
		if longFlagMode {
			return "", nil, false
		}
	}
	return v, nil, true
}
