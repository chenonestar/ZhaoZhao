//go:build !windows

package extract

import "image"

// wicDecodeImpl：只有 Windows 有自带的图片解码组件。
func wicDecodeImpl(p string, from int, fn func(i, n int, img *image.Gray) error) error {
	return errNoWIC
}
