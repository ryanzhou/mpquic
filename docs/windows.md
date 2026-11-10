# Windows: service and configurator

The Windows release (`mpquic-windows-amd64.exe`, `mpquic-windows-arm64.exe`)
is a single executable with three roles:

* **Command line:** `mpquic.exe local|tunnel|remote|relay ...` works exactly
  as on Linux (Ctrl-C to stop).
* **Configurator:** double-click the exe (or run `mpquic.exe gui`) to open a
  GUI that edits the settings, installs and updates the Windows service,
  starts, stops and restarts it, and shows its log. It asks for administrator
  rights (UAC) when it starts.
* **Windows service:** each instance runs one role (`local`, `tunnel`,
  `remote` or `relay`) from a config file, starts at boot, and is restarted
  automatically if it fails.

From an elevated prompt, the same can be scripted. `service install` takes
the role and the same options as the foreground command:

```bat
mpquic.exe service install remote -p 8388 -k long-random-password -u
mpquic.exe service install local -s remote.example.com:8388 -s relay-a.example.net:9000 -k long-random-password -u
mpquic.exe service install --name relay relay --listen :9000 --target remote.example.com:8388
:: or from an existing JSON config:
mpquic.exe service install local -c C:\path\to\local.json
mpquic.exe service status | start | stop | restart  [--name NAME]
mpquic.exe service uninstall [--name NAME] [--purge]
```

| | |
|---|---|
| binary | `%ProgramFiles%\MPQUIC\mpquic.exe`. `install` copies the running exe there. To upgrade, run the new exe's `service install` (or *Save and apply* in the GUI) |
| config | `%ProgramData%\MPQUIC\NAME.json` (the usual JSON format plus `"role"`). Readable only by Administrators, SYSTEM and the service |
| log | `%ProgramData%\MPQUIC\logs\NAME.log`, rotated at 10 MB. Start failures also go to the Application event log |
| account | `NT AUTHORITY\LocalService`. TLS `cert`/`key` files must be readable by it (for example, put them in `%ProgramData%\MPQUIC`) |
| firewall | `install` adds an inbound Windows Firewall rule for the QUIC/relay UDP ports. For SOCKS5/tunnel listeners, only when bound to a non-loopback address |

`NAME` defaults to `MPQUIC`. Use `--name` to run several instances, for
example a relay next to a local. The Windows build is console-mode. On
Windows 11 24H2 and later, double-clicking opens no console window. On
earlier versions, a console window flashes briefly before the configurator
appears.

The configurator keeps a `wireguard` section it finds in the config but does
not edit it (edit the JSON file); the service installer opens the WireGuard
UDP port in the firewall. The TUN interface is not available on Windows.
