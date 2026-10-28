//go:build !windows

package extract

import (
	"errors"
	"image"
	"time"
)

var errNoWinOCR = errors.New("只有 Windows 上有")

// WinOCRAvailable 说明这台电脑能不能用 Windows 自带的 OCR 认中文。
func WinOCRAvailable() error { return errNoWinOCR }

func winOCRImage(img image.Image, timeout time.Duration) (string, error) { return "", errNoWinOCR }
