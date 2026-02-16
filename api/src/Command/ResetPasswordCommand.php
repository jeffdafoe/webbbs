<?php

namespace App\Command;

use App\Repository\UserRepository;
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Component\Console\Attribute\AsCommand;
use Symfony\Component\Console\Command\Command;
use Symfony\Component\Console\Input\InputArgument;
use Symfony\Component\Console\Input\InputInterface;
use Symfony\Component\Console\Output\OutputInterface;
use Symfony\Component\Console\Style\SymfonyStyle;
use Symfony\Component\PasswordHasher\Hasher\UserPasswordHasherInterface;

#[AsCommand(
    name: 'zbbs:reset-password',
    description: 'Reset a user password',
)]
class ResetPasswordCommand extends Command
{
    public function __construct(
        private UserRepository $userRepository,
        private UserPasswordHasherInterface $passwordHasher,
        private EntityManagerInterface $entityManager,
    ) {
        parent::__construct();
    }

    protected function configure(): void
    {
        $this->addArgument('username', InputArgument::REQUIRED, 'The username to reset');
    }

    protected function execute(InputInterface $input, OutputInterface $output): int
    {
        $io = new SymfonyStyle($input, $output);
        $username = $input->getArgument('username');

        $user = $this->userRepository->findOneBy(['username' => $username]);
        if ($user === null) {
            $io->error('User "' . $username . '" not found.');
            return Command::FAILURE;
        }

        $io->text('Resetting password for "' . $username . '"');
        $io->newLine();

        while (true) {
            $password = $io->ask('New password (visible)', null, function (?string $value): string {
                if ($value === null || strlen($value) < 4) {
                    throw new \RuntimeException('Password must be at least 4 characters.');
                }
                return $value;
            });

            $confirm = $io->ask('Confirm password');

            if ($password === $confirm) {
                break;
            }

            $io->warning('Passwords do not match. Try again.');
        }

        $user->setPassword($this->passwordHasher->hashPassword($user, $password));
        $this->entityManager->flush();

        $io->success('Password reset for "' . $username . '".');

        return Command::SUCCESS;
    }
}
