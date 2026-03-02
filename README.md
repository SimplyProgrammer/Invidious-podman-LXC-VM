# Invidious-podman-LXC-VM
Simple repo for fast and automated deployment of local Invidious for Proxmox LXCs or VMs, utilizing podman.
Inspired by https://github.com/NapoleonWils0n/cerberus/blob/master/invidious/invidious-2025.org

## Instalation
Create a Debian 12+ VM or an unprivileged Debian 12+ LXC container with:
* Name "invidious" (or something similar)
* 4GB+ of RAM
* 2+ x86_64 cores, 4 is ideal (ARM is also possible but `compose.yml` has to be modified accordingly).
* 20+GB of storage

Feel free to modify the `compose.yml` file when you know what you are doing and see it asfitting. 

Now you have to make your container have a static IP or configure a static lease on your router.

Further, you will need a DNS domain, paid or local one to point to this IP of your LXC/VM with Invidious. 
It would be great if your router would allow/do this by default, and automatically assign something like `invidious.lan` or even better, `invidious.home.arpa` to your LXC/VM. For example, [OpenWrt](https://forum.openwrt.org/t/use-home-arpa-as-default-tld-for-local-network/165056/11) does this by default. For other routers, extra config might be needed.
If you have your own domain, reversed-proxy etc., setup might differ for you...

Run this script (with sudo privileges or as root):
```
mkdir -p ~/invidious-podman && cd "$_" && git clone "https://github.com/SimplyProgrammer/Invidious-podman-LXC-VM.git" . && chmod 755 setup-invidious.sh && ./setup-invidious.sh
```
When it asks for the Invidious git repo, for 90% of you just press enter (unless you have your own forked one).

The most important part, when it asks for the DNS domain, specified aformentioned `invidious.lan`, `invidious.home.arpa` or whatever you have. But make sure the domain is reachable and working in advance (`nslookup` etc.).

If everything was done properly, now you should have a working LXC/VM with Invidious service and should be able to open it at `https://<your-invidious-domain>/`.\
It should automatically start with your VM/LXC, no extra work needed.

# Notice
This solution was intended for Proxmox VMs and LXC but in theory it should work with any platform that can virtualize Linux with basic networking included.

If you find this helpfull it would be great if you could start the repo and spread the word.

If you encounter any problem, consider opening an issue.
