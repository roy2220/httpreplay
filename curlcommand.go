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
		if flagValue, err, ok := matchCurlFlag(arg, "-X", "--request", popNextArg); ok {
			if err != nil {
				return curlCommand{}, err
			}
			curlCommand1.Request = flagValue
			continue
		}
		if flagValue, err, ok := matchCurlFlag(arg, "-H", "--header", popNextArg); ok {
			if err != nil {
				return curlCommand{}, err
			}
			key, _, ok := strings.Cut(flagValue, ":")
			if !ok {
				return curlCommand{}, fmt.Errorf("invalid header: %v", flagValue)
			}
			if strings.EqualFold(key, "Content-Type") {
				contentTypeHeaderIsSet = true
			}
			header := curlCommand1.Header
			if header == nil {
				header = new(bytes.Buffer)
				curlCommand1.Header = header
			} else {
				header.WriteString("\r\n")
			}
			header.WriteString(flagValue)
			continue
		}
		if flagValue, err, ok := matchCurlFlag(arg, "-d", "--data", popNextArg); ok {
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
			data.WriteString(flagValue)
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

func matchCurlFlag(arg, shortFlagName, longFlagName string, popNextArg func() (string, bool)) (string, error, bool) {
	var (
		flagMode  byte
		flagValue string
	)
	switch {
	case strings.HasPrefix(arg, shortFlagName):
		flagMode = 'S'
		flagValue = arg[len(shortFlagName):]
	case strings.HasPrefix(arg, longFlagName):
		flagMode = 'L'
		flagValue = arg[len(longFlagName):]
	default:
		return "", nil, false
	}
	if flagValue == "" {
		var ok bool
		flagValue, ok = popNextArg()
		if !ok {
			return "", fmt.Errorf("missing flag value for %v/%v", shortFlagName, longFlagName), true
		}
	} else {
		if flagValue[0] == '=' {
			flagValue = flagValue[1:]
		} else {
			if flagMode == 'L' {
				return "", nil, false
			}
		}
	}
	return flagValue, nil, true
}
