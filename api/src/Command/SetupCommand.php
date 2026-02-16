<?php

namespace App\Command;

use App\Entity\Setting;
use App\Service\UserService;
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Component\Console\Attribute\AsCommand;
use Symfony\Component\Console\Command\Command;
use Symfony\Component\Console\Input\InputInterface;
use Symfony\Component\Console\Output\OutputInterface;
use Symfony\Component\Console\Style\SymfonyStyle;

#[AsCommand(
    name: 'zbbs:setup',
    description: 'Initial BBS setup — creates the sysop account and configures settings',
)]
class SetupCommand extends Command
{
    public function __construct(
        private UserService $userService,
        private EntityManagerInterface $entityManager,
    ) {
        parent::__construct();
    }

    protected function execute(InputInterface $input, OutputInterface $output): int
    {
        $io = new SymfonyStyle($input, $output);

        $io->title('ZBBS Setup');

        // Check if a sysop already exists
        $connection = $this->entityManager->getConnection();
        $existingSysop = $connection->fetchOne(
            "SELECT id FROM \"user\" WHERE roles::text LIKE '%ROLE_SYSOP%' LIMIT 1"
        );

        if ($existingSysop !== false) {
            $io->error('A sysop account already exists. Setup has already been completed.');
            return Command::FAILURE;
        }

        $io->section('BBS Configuration');

        $bbsName = $io->ask('BBS Name', 'ZBBS');
        $tagline = $io->ask('Tagline (short subtitle shown on the welcome screen)', 'Bulletin Board System');
        $phone = $io->ask('Phone number (shown in modem disconnect)', '555-ZBBS');

        $io->section('Sysop Account');

        $username = $io->ask('Sysop username', null, function (?string $value): string {
            if ($value === null || trim($value) === '') {
                throw new \RuntimeException('Username cannot be empty.');
            }
            if (strlen($value) > 35) {
                throw new \RuntimeException('Username cannot exceed 35 characters.');
            }
            return $value;
        });

        while (true) {
            $password = $io->askHidden('Sysop password', function (?string $value): string {
                if ($value === null || strlen($value) < 4) {
                    throw new \RuntimeException('Password must be at least 4 characters.');
                }
                return $value;
            });

            $confirm = $io->askHidden('Confirm password');

            if ($password === $confirm) {
                break;
            }

            $io->warning('Passwords do not match. Try again.');
        }

        // Create sysop account
        $io->section('Creating...');

        $this->userService->createUser([
            'username' => $username,
            'password' => $password,
            'roles' => ['ROLE_SYSOP'],
        ]);

        $io->text('Sysop account created.');

        // Insert settings (only if they don't already exist)
        $defaultSettings = [
            ['bbs_name', $bbsName, 'Name of the BBS', true],
            ['bbs_tagline', $tagline, 'Tagline shown on welcome screen', true],
            ['bbs_phone', $phone, 'Phone number shown in modem disconnect', true],
            ['default_role', 'ROLE_USER', 'Default role assigned to new users', false],
            ['registration_enabled', 'true', 'Whether new user registration is open', true],
            ['terminal_border_color', '#1a1a2e', 'Border color around the terminal (HTML color code)', true],
        ];

        foreach ($defaultSettings as [$key, $value, $description, $isPublic]) {
            $existing = $this->entityManager->getRepository(Setting::class)->find($key);
            if ($existing === null) {
                $setting = new Setting($key, $value);
                $setting->setDescription($description);
                $setting->setIsPublic($isPublic);
                $this->entityManager->persist($setting);
            }
        }
        $this->entityManager->flush();

        $io->text('Settings configured.');

        $io->newLine();
        $io->success('ZBBS setup complete! You can now log in as ' . $username . '.');

        return Command::SUCCESS;
    }
}
