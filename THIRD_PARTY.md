# Third-party material

foxess-local is licensed under the GNU General Public License, version 3
or (at your option) any later version — see [LICENSE](LICENSE).

## foxess_modbus register maps

`internal/fmdata/foxess_modbus.json` is generated (by
`tools/fmimport/fmimport.py`) from the register maps of
[foxess_modbus](https://github.com/nathanmarlor/foxess_modbus), used under
the MIT License:

```
MIT License

Copyright (c) 2021 nathanmarlor

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## Go modules

The binary links these modules, each under its own permissive licence
(their licence files are in the Go module cache; a release should ship
them alongside the binary):

| Module | Licence |
|---|---|
| github.com/nicois/prox | Apache-2.0 |
| github.com/BurntSushi/toml | MIT |
| github.com/dustin/go-humanize | MIT |
| github.com/google/uuid | BSD-3-Clause |
| gopkg.in/yaml.v3 | MIT and Apache-2.0 |
| golang.org/x/crypto, golang.org/x/sys | BSD-3-Clause |
| modernc.org/sqlite, libc, mathutil, memory; github.com/remyoudompheng/bigfft | BSD-3-Clause |

Apache-2.0 is compatible with GPL version 3 (not version 2), which is one
reason this project is GPLv3.
