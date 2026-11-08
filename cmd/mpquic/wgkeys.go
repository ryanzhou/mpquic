package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"

	"mpquic/internal/wg"
)

// wgKeygen prints a new WireGuard key pair.
func wgKeygen(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("wg-keygen takes no arguments")
	}
	k, err := wg.GenerateKey()
	if err != nil {
		return err
	}
	fmt.Printf("PrivateKey = %s\nPublicKey = %s\n", k, k.PublicKey())
	return nil
}

// wgPubkey reads a private key on stdin and prints its public key, like
// "wg pubkey".
func wgPubkey(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("wg-pubkey takes no arguments (it reads the private key on stdin)")
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("wg-pubkey: no private key on stdin")
	}
	k, err := wg.ParseKey(line)
	if err != nil {
		return err
	}
	fmt.Println(k.PublicKey())
	return nil
}
