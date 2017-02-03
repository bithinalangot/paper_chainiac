#!/usr/bin/env python3.5

try:
    from bs4 import BeautifulSoup
    from urllib.request import urlopen
    import random
    import subprocess
    import templates
    from datetime import datetime, timedelta
    import psutil
    import csv, sys, os, time
except:
    raise

simulate = False
simulateSuccess = False
url_repro_build = 'https://tests.reproducible-builds.org'
url_repro_testing = 'https://tests.reproducible-builds.org/debian/rb-pkg/testing/amd64/'

packages_required = ['acl', 'attr', 'base-files', 'base-passwd', 'coreutils', 'debconf', 'debianutils', 'diffutils',
                     'dpkg', 'e2fsprogs', 'findutils', 'grep', 'gzip', 'init-system-helpers', 'libselinux', 'libsepol',
                     'lsb', 'mawk',  'pam', 'pcre3', 'sed', 'sysvinit', 'tar', 'tzdata', 'util-linux', 'zlib']

packages_essential = ['debianutils', 'diffutils', 'e2fsprogs', 'findutils', 'perl', 'sysvinit', 'tar']

packages_popular = ['hostname', 'netbase', 'adduser', 'tzdata', 'bsdmainutils', 'cpio', 'logrotate',
                    'debian-archive-keyring', 'liblocale-gettext-perl', 'net-tools', 'ucf', 'popularity-contest',
                    'cron', 'manpages', 'libtext-wrapi18n-perl', 'iptables', 'ifupdown', 'man-db', 'mime-support',
                    'pciutils', 'libxml2', 'initramfs-tools', 'libcap2', 'dmidecode', 'busybox', 'file', 'less',
                    'ca-certificates', 'psmisc', 'nano', 'tasksel', 'insserv', 'installation-report', 'laptop-detect',
                    'linux-base', 'xml-core', 'aptitude', 'bzip2', 'os-prober', 'acpid', 'discover-data',
                    'bash-completion', 'dictionaries-common', 'eject', 'kmod', 'whois', 'iso-codes', 'geoip-database',
                    'bc', 'acpi']

packages_random = ['golang-github-hlandau-xlog', 'cal', 'libpath-dispatcher-declarative-perl', 'lunar-date', 'pmailq',
                   'aolserver4-nsxml', 'node-tilelive-vector', 'golang-github-hashicorp-go-getter', 'yacpi',
                   'libdata-stag-perl', 'libnet-oauth2-perl', 'libjs-jquery-dotdotdot', 'libclass-c3-adopt-next-perl',
                   'libobject-remote-perl', 'libxml-rsslite-perl', 'python-click-log', 'cl-salza2',
                   'globus-ftp-control', 'childsplay-alphabet-sounds-sl', 'fgetty', 'xmlextras', 'node-superagent',
                   'django-memoize', 'libtemplate-plugin-stash-perl', 'systraq', 'libtpl',
                   'libdist-zilla-plugin-config-git-perl', 'php-doctrine-cache-bundle', 'tz-converter', 'hackrf',
                   'slice', 'xfce4-taskmanager', 'sshfs-fuse', 'node-simplesmtp', 'visionegg',
                   'haskell-mutable-containers', 'gvfs', 'qdacco', 'haskell-ghc-events', 'ply', 'dymo-cups-drivers',
                   'ruby-bacon', 'liblinux-usermod-perl', 'puppet-module-puppetlabs-postgresql', 'jalview', 'masscan',
                   'octave-gsl', 'geronimo-ejb-3.2-spec', 'haskell-pcap', 'exuberant-ctags']

# Modifier for a dependency line
def parse_dpnd(li):
    li = li.replace(' ', '')
    li = li.replace(',', ' ')
    li = li.replace(')', '')
    li = li.replace('(', '')

    return li

dlog = None

def docker(cmd, args=[]):
    cmds = ['docker'] + cmd.split(" ") + args
    if dlog:
        dlog.write(('\n\n--> %s\n\n' % '::'.join(cmds)).encode('utf-8'))
        dlog.flush()
        proc = subprocess.Popen(cmds, stdout=dlog, stderr=dlog)
        proc.wait()
        return ""
    else:
        # print('--> %s' % '::'.join(cmds))
        proc = subprocess.Popen(cmds, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE)
        proc.wait()
        return proc.stdout.read().decode('utf-8').strip() +\
               proc.stderr.read().decode('utf-8').strip()
        # out = subprocess.check_output(cmds)

def docker_build():
    if not "repro_build" in docker('images repro_build'):
        docker('build -t repro_build .')

def docker_exec(name, cmd):
    return docker('exec %s bash -c' % name, [cmd])

def docker_run(pkg):
    global dlog
    dname = '-'.join(['repro', pkg, str(os.getpid())])
    did = docker('run --name=%s -d repro_build bash -c' % dname,["sleep 3600"])
    dlog = open(pkg + '.log', 'wb')
    time.sleep(1)
    docker_exec(dname, "echo 193.62.202.30 snapshot.debian.org >> /etc/hosts")
    return dname, did

# Build a container from the docker file and retrieve hash of the binary
def compile_bin(dname, did, dependencies, version, short_version, bina):
    global dlog
    comhash = ''
    wall_start_time = time.perf_counter()
    cpu_user_start, cpu_system_start = psutil.cpu_times().user, psutil.cpu_times().system
    if not simulate:
        docker_exec(dname, "apt-get install -y " + ' '.join(dependencies))
        full_pkg = name + '=' + version
        docker_exec(dname, "apt-get source " + full_pkg)
        docker_exec(dname, "apt-get build-dep -y --force-yes " + full_pkg)
        src_dir = dir + '-' + short_version.partition('-')[0] + '/'
        docker_exec(dname, "cd %s; dpkg-buildpackage -us -uc -tc -j8" % src_dir)

    cpu_user, cpu_system = psutil.cpu_times().user - cpu_user_start, psutil.cpu_times().system - cpu_system_start
    wall_time = time.perf_counter() - wall_start_time
    ddir = os.path.join('/sys/fs/cgroup/cpuacct/docker', did, 'cpuacct.stat')
    if os.path.isfile(ddir):
        with open(ddir, 'r') as f:
            user = float(f.readline().strip().split(" ")[1])/100.
            system = float(f.readline().strip().split(" ")[1])/100.
    else:
        # No sysfs, probably MacOS, use system CPU time.
        user = cpu_user
        system = cpu_system

    try:
        dlog.close()
        dlog = None
        sha_cmd = 'sha256sum ' + bina
        comhash = docker_exec(dname, sha_cmd).split(' ')[0]
        docker('rm -f %s' % dname)
    except:
        print("Some error while building", name, sys.exc_info()[0])

    # print("User docker/psutil: %f/%f - System docker/psutil: %f/%f" %
    #       (user, cpu_user, system, cpu_system))
    return comhash, round(wall_time, 3), round(user, 3), round(system, 3)


# Find and add two Debian snapshots preceding the build time and one
# following the build time
def find_snapshots(pkg, name):
    page = urlopen(url_repro_testing + pkg + '.html').read()
    soup = BeautifulSoup(page, 'html.parser')
    timestr = soup.body.header.find('span', {'class': 'build-time'}).string.split()[1:3]
    build_time = datetime.strptime(' '.join(timestr), '%Y-%m-%d %H:%M')

    snapbuf = []
    snapbuf_future = []
    for day in [-1,0,1]:
        atime = build_time + timedelta(days=day)
        # print("Fetching", atime)
        snappage = urlopen(
            'http://snapshot.debian.org/archive/debian/?year=%s;month=%s' %
            ( atime.strftime('%Y'), atime.strftime('%m'))).read()
        snapsoup = BeautifulSoup(snappage, 'html.parser')
        for snapshot in snapsoup.body.p.find_all('a'):
            snaptime = datetime.strptime(snapshot.string, '%Y-%m-%d %H:%M:%S')
            if snaptime < build_time:
                snapbuf.append(snapshot)  # snapbuf is a list of snapshots before the build
            else:
                snapbuf_future.append(snapshot)
    # Also get the first snapshot in the future
    snapbuf.append(snapbuf_future[0])

    # Adding retrieved snapshots as sources to Dockerfile
    snap_url = "http://snapshot.debian.org"
    # snap_url = "http://icsil1-conode1.epfl.ch:3142/snapshot.debian.org"
    # snap_url = "http://icsil1-conode1.epfl.ch:3128/"
    if len(snapbuf) < 3:
        print("The build is done before the first snapshot of the month!")
    else:
        for snap in snapbuf[-3:]:
            for d in ['deb', 'deb-src']:
                line = '%s %s/archive/debian/%s stretch main' % \
                       (d, snap_url, snap['href'])
                docker_exec(name, 'echo %s >> /etc/apt/sources.list' % line)
    docker_exec(name, "echo 'Acquire::Check-Valid-Until \"false\";' >> /etc/apt/apt.conf" )
    docker_exec(name, 'cat /etc/apt/sources.list')
    if not simulate:
        docker_exec(name, 'apt-get update')

# Save build results into csv file
def save_results(yes, no, fail):
    with open('reprotest.csv', 'w') as f:
        csvf = csv.writer(f)
        csvf.writerow(['package', 'binary', 'size', 'wall_time', 'cpu_user_time', 'cpu_system_time', 'outcome'])
        for group in yes, no, fail:
            for pckg in group:
                csvf.writerow(pckg)


# Returns a set of packages to be built
def get_packages(option):
    packs = []

    if option == 'required':
        packs = packages_required

    elif option == 'essential':
        packs = packages_essential

    elif option == 'popular':
        packs = packages_popular

    elif option == 'random':
        packs = packages_random

    elif option == 'random_fresh':
        SET_SIZE = 3
        allpacks = []
        url = 'https://tests.reproducible-builds.org/debian/testing/amd64/index_reproducible.html'
        content = urlopen(url).read()
        soup = BeautifulSoup(content, 'html.parser')
        for p in soup.body.div.find('code').find_all('a', class_='package'):
            allpacks.append(p.string)
        packs = random.sample(allpacks, SET_SIZE)

    elif option == 'cli':
        packs = [sys.argv[2]]

    return packs


packages = get_packages(sys.argv[1])
hash_match, hash_differ, failed = [], [], []
docker_build()

def find_dependencies(pkg):
    page = urlopen(url_repro_testing + pkg + '.html').read()
    soup = BeautifulSoup(page, 'html.parser')
    page = urlopen(url_repro_build + soup.body.header.find('a', {'title': 'Show: build info'})['href']).read()
    soup = BeautifulSoup(page, 'html.parser')
    lines = str(soup).split('\n')
    shaflag = dflag = False
    version = name = sha = binary = dir = short_version = size = ''
    i = 0
    dependencies = []
    for line in lines:  # Extracting describing data
        if not dflag:  # Parse data about the package at first
            words = line.split()
            if len(words):
                if words[0] == 'Version:':
                    if ':' in words[1]:
                        version = words[1]
                        short_version = words[1].split(':')[1]
                    else:
                        version = short_version = words[1]
                elif words[0] == 'Binary:':  # To know binary name for the verified package
                    if pkg in words:
                        name = pkg
                    elif (pkg + '1') in words:
                        name = pkg + '1'
                    else:
                        name = words[1]
                elif words[0] == 'Source:':  # To know a name of the folder where to compile a package
                    dir = words[1]
            if shaflag == True:
                if (len(words) > 2) and (name + '_' + short_version in words[2] and '.deb' in words[2]):
                    binary = words[2]
                    sha = words[0]
                    size = words[1]
                    shaflag = False
            if line == 'Checksums-Sha256:':
                shaflag = True
            if line == 'Installed-Build-Depends:':  # All the describing data is found, move to dependencies
                dflag = True
        else:
            # Parsing dependencies
            i += 1
            dependencies.append(parse_dpnd(line))

    return dependencies, version, name, sha, binary, dir, short_version, size


for pkg in packages:
    pkgstr = sys.argv[1]+'/' + pkg
    # print("Reproducibly building package:", pkgstr)

    dname, did = docker_run(pkg)

    # Get all snapshots
    find_snapshots(pkg, dname)

    # Find all the dependencies
    dependencies, version, name, sha, binary, dir, short_version, size = \
        find_dependencies(pkg)

    if not simulate:
        computed_hash, wtime, utime, stime = \
        compile_bin(dname, did, dependencies, version, short_version, binary)
    else:
        computed_hash, wtime, utime, stime = "", 1.5, 2.5, 3.5
        if simulateSuccess:
            computed_hash = sha
        print("Got", computed_hash, wtime, utime, stime)

    if sha != '' and computed_hash == sha:
        hash_match.append((pkg, binary, size, wtime, utime, stime, 'y'))
        print("Hashes match for %s: wall(%f) user(%f) system(%f)" %
              (pkgstr, wtime, utime, stime))
    else:
        if computed_hash == '':
            failed.append((pkg, binary, size, wtime, utime, stime, 'f'))
            print('Fail in the build process')
        else:
            hash_differ.append((pkg, binary, size, wtime, utime, stime, 'n'))
            print("Hashes differ for", pkgstr, computed_hash, "and", sha)

save_results(hash_match, hash_differ, failed)

print('Built packages with matching hash: ', hash_match)
print('Failed to build:', failed)
print('Built packages with differed hash:', hash_differ)

if sys.argv[1] == 'cli':
    if len(hash_match) > 0:
        print(sys.argv[2], hash_match[0])
        print("Success", wtime, utime, stime)
    else:
        print("Failed")
