package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestInstalledFileManagers(t *testing.T) {
	installed := func(name string) (string, error) {
		switch name {
		case "nautilus", "dolphin", "thunar":
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	cases := []struct {
		desktop string
		want    []string
	}{
		{"KDE", []string{"dolphin", "nautilus", "thunar"}},
		{"ubuntu:GNOME", []string{"nautilus", "dolphin", "thunar"}},
		{"XFCE", []string{"thunar", "nautilus", "dolphin"}},
		{"", []string{"nautilus", "dolphin", "thunar"}},
		{"Hyprland", []string{"nautilus", "dolphin", "thunar"}},
	}
	for _, c := range cases {
		if got := installedFileManagers(c.desktop, installed); !reflect.DeepEqual(got, c.want) {
			t.Errorf("desktop %q: got %v, want %v", c.desktop, got, c.want)
		}
	}
	none := func(string) (string, error) { return "", errors.New("not found") }
	if got := installedFileManagers("GNOME", none); len(got) != 0 {
		t.Errorf("nothing installed: got %v", got)
	}
}
