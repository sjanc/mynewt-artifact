/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

// Decoder for PKCS#5 encrypted PKCS#8 private keys.
package sec

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"hash"

	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/ssh/terminal"
)

var (
	oidPbes2          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPbkdf2         = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHmacWithSha1   = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHmacWithSha224 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 8}
	oidHmacWithSha256 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidAes128CBC      = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAes256CBC      = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
)

// We only support a narrow set of possible key types, namely the type
// generated by either MCUboot's `imgtool.py` command, or using an
// OpenSSL command such as:
//
//     openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
//         -aes-256-cbc > keyfile.pem
//
// or similar for ECDSA.  Specifically, the encryption must be done
// with PBES2, and PBKDF2, and aes-256-cbc used as the cipher.
type pkcs5 struct {
	Algo      pkix.AlgorithmIdentifier
	Encrypted []byte
}

// The parameters when the algorithm in pkcs5 is oidPbes2
type pbes2 struct {
	KeyDerivationFunc pkix.AlgorithmIdentifier
	EncryptionScheme  pkix.AlgorithmIdentifier
}

// Salt is given as a choice, but we will only support the inlined
// octet string.
type pbkdf2Param struct {
	Salt      []byte
	IterCount int
	HashFunc  pkix.AlgorithmIdentifier
	// Optional and default values omitted, and unsupported.
}

type hashFunc func() hash.Hash

func parseEncryptedPrivateKey(der []byte) (key interface{}, err error) {
	var wrapper pkcs5
	if _, err = asn1.Unmarshal(der, &wrapper); err != nil {
		return nil, err
	}
	if !wrapper.Algo.Algorithm.Equal(oidPbes2) {
		return nil, fmt.Errorf("pkcs5: Unknown PKCS#5 wrapper algorithm: %v", wrapper.Algo.Algorithm)
	}

	var pbparm pbes2
	if _, err = asn1.Unmarshal(wrapper.Algo.Parameters.FullBytes, &pbparm); err != nil {
		return nil, err
	}
	if !pbparm.KeyDerivationFunc.Algorithm.Equal(oidPbkdf2) {
		return nil, fmt.Errorf("pkcs5: Unknown KDF: %v", pbparm.KeyDerivationFunc.Algorithm)
	}

	var kdfParam pbkdf2Param
	if _, err = asn1.Unmarshal(pbparm.KeyDerivationFunc.Parameters.FullBytes, &kdfParam); err != nil {
		return nil, err
	}

	var hashNew hashFunc
	switch {
	case kdfParam.HashFunc.Algorithm.Equal(oidHmacWithSha1):
		hashNew = sha1.New
	case kdfParam.HashFunc.Algorithm.Equal(oidHmacWithSha224):
		hashNew = sha256.New224
	case kdfParam.HashFunc.Algorithm.Equal(oidHmacWithSha256):
		hashNew = sha256.New
	default:
		return nil, fmt.Errorf("pkcs5: Unsupported hash: %v", pbparm.EncryptionScheme.Algorithm)
	}

	// Get the encryption used.
	size := 0
	var iv []byte
	switch {
	case pbparm.EncryptionScheme.Algorithm.Equal(oidAes256CBC):
		size = 32
		if _, err = asn1.Unmarshal(pbparm.EncryptionScheme.Parameters.FullBytes, &iv); err != nil {
			return nil, err
		}
	case pbparm.EncryptionScheme.Algorithm.Equal(oidAes128CBC):
		size = 16
		if _, err = asn1.Unmarshal(pbparm.EncryptionScheme.Parameters.FullBytes, &iv); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("pkcs5: Unsupported cipher: %v", pbparm.EncryptionScheme.Algorithm)
	}

	return unwrapPbes2Pbkdf2(&kdfParam, size, iv, hashNew, wrapper.Encrypted)
}

func unwrapPbes2Pbkdf2(param *pbkdf2Param, size int, iv []byte, hashNew hashFunc, encrypted []byte) (key interface{}, err error) {
	pass, err := getPassword()
	if err != nil {
		return nil, err
	}
	cryptoKey := pbkdf2.Key(pass, param.Salt, param.IterCount, size, hashNew)

	block, err := aes.NewCipher(cryptoKey)
	if err != nil {
		return nil, err
	}
	enc := cipher.NewCBCDecrypter(block, iv)

	plain := make([]byte, len(encrypted))
	enc.CryptBlocks(plain, encrypted)

	plain, err = checkPkcs7Padding(plain)
	if err != nil {
		return nil, err
	}

	return x509.ParsePKCS8PrivateKey(plain)
}

// Verify that PKCS#7 padding is correct on this plaintext message.
// Returns a new slice with the padding removed.
func checkPkcs7Padding(buf []byte) ([]byte, error) {
	if len(buf) < 16 {
		return nil, fmt.Errorf("Invalid padded buffer")
	}

	padLen := int(buf[len(buf)-1])
	if padLen < 1 || padLen > 16 {
		return nil, fmt.Errorf("Invalid padded buffer")
	}

	if padLen > len(buf) {
		return nil, fmt.Errorf("Invalid padded buffer")
	}

	for pos := len(buf) - padLen; pos < len(buf); pos++ {
		if int(buf[pos]) != padLen {
			return nil, fmt.Errorf("Invalid padded buffer")
		}
	}

	return buf[:len(buf)-padLen], nil
}

// For testing, a key can be set here.  If this is empty, the key will
// be queried via prompt.
var KeyPassword = []byte{}

// Prompt the user for a password, unless we have stored one for
// testing.
func getPassword() ([]byte, error) {
	if len(KeyPassword) != 0 {
		return KeyPassword, nil
	}

	fmt.Printf("key password: ")
	return terminal.ReadPassword(0)
}
